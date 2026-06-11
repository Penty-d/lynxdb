package rest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// ingestLeveledEvents ingests n JSON events with explicit level/service fields
// so view filters and aggregations have structured columns to work with.
// Events alternate level error/info and cycle through serviceCount services.
func ingestLeveledEvents(t *testing.T, addr string, n, serviceCount int) {
	t.Helper()
	now := float64(time.Now().Unix())
	events := make([]map[string]interface{}, n)
	for i := 0; i < n; i++ {
		level := "info"
		if i%2 == 0 {
			level = "error"
		}
		events[i] = map[string]interface{}{
			"time":  now + float64(i),
			"event": fmt.Sprintf("request %d", i),
			"index": "main",
			"fields": map[string]interface{}{
				"level":   level,
				"service": fmt.Sprintf("svc-%02d", i%serviceCount),
			},
		}
	}
	body, _ := json.Marshal(events)
	resp, err := http.Post(fmt.Sprintf("http://%s/api/v1/ingest", addr), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST events: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ingest status: %d", resp.StatusCode)
	}
}

// createViewAndWaitActive creates a materialized view and polls until its
// backfill completes and the status is active.
func createViewAndWaitActive(t *testing.T, addr, name, query string) {
	t.Helper()
	createBody, _ := json.Marshal(map[string]interface{}{
		"name":  name,
		"query": query,
	})
	resp, err := http.Post(fmt.Sprintf("http://%s/api/v1/views", addr), "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("create view: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create view status: %d, body: %s", resp.StatusCode, b)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://%s/api/v1/views/%s", addr, name))
		if err != nil {
			t.Fatalf("get view: %v", err)
		}
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		data, _ := result["data"].(map[string]interface{})
		if status, _ := data["status"].(string); status == "active" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("view %s did not become active within deadline", name)
}

// postSyncQuery executes a sync query and returns the decoded response body.
// A relative from bound is set so the request bypasses the result cache
// (dynamic time bounds disable caching by design).
func postSyncQuery(t *testing.T, addr, query string) map[string]interface{} {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"q": query, "from": "-24h"})
	resp, err := http.Post(fmt.Sprintf("http://%s/api/v1/query", addr), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST query: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("query status: %d, body: %s", resp.StatusCode, raw)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode query response: %v\nbody: %s", err, raw)
	}
	return result
}

// TestViews_QueryViewByName verifies that `from <viewname>` reads materialized
// view rows on the LynxFlow path. Regression test for the RFC-002 gap where
// view names were resolved as (nonexistent) indexes, returning empty results.
func TestViews_QueryViewByName(t *testing.T) {
	srv, _, cleanup := startDiskTestServer(t)
	defer cleanup()

	ingestLeveledEvents(t, srv.Addr(), 20, 4)
	createViewAndWaitActive(t, srv.Addr(), "mv_err_by_svc",
		`from main | where level == "error" | stats count() by service`)

	result := postSyncQuery(t, srv.Addr(), `from mv_err_by_svc | sort service`)
	data, _ := result["data"].(map[string]interface{})
	if data == nil {
		t.Fatalf("missing data: %v", result)
	}
	// 20 events, 10 errors across 4 services -> some rows per service.
	var rowCount int
	switch data["type"] {
	case "aggregate":
		rows, _ := data["rows"].([]interface{})
		rowCount = len(rows)
	default:
		events, _ := data["events"].([]interface{})
		rowCount = len(events)
	}
	if rowCount == 0 {
		t.Fatalf("query over view returned no rows: %v", data)
	}
}

// TestViews_AcceleratedQueryDifferential verifies that a long-form query
// matching a materialized view is (a) answered identically to the
// non-accelerated execution and (b) annotated with meta.stats.accelerated_by.
func TestViews_AcceleratedQueryDifferential(t *testing.T) {
	srv, _, cleanup := startDiskTestServer(t)
	defer cleanup()

	ingestLeveledEvents(t, srv.Addr(), 40, 4)

	query := `from main | where level == "error" | stats count() by service | sort service`

	// Baseline: no view exists yet, so this runs against raw data.
	baseline := postSyncQuery(t, srv.Addr(), query)
	baselineData, _ := baseline["data"].(map[string]interface{})
	if baselineData == nil {
		t.Fatalf("baseline missing data: %v", baseline)
	}

	createViewAndWaitActive(t, srv.Addr(), "mv_accel_diff",
		`from main | where level == "error" | stats count() by service`)

	// Accelerated: same query should now be rewritten to read the view.
	accel := postSyncQuery(t, srv.Addr(), query)
	accelData, _ := accel["data"].(map[string]interface{})
	if accelData == nil {
		t.Fatalf("accelerated missing data: %v", accel)
	}

	// Differential: identical results with and without acceleration.
	if !reflect.DeepEqual(baselineData["rows"], accelData["rows"]) ||
		!reflect.DeepEqual(baselineData["columns"], accelData["columns"]) {
		t.Fatalf("accelerated results differ from baseline\nbaseline: %v\naccel:    %v",
			baselineData, accelData)
	}

	// Acceleration must be visible in meta.
	meta, _ := accel["meta"].(map[string]interface{})
	stats, _ := meta["stats"].(map[string]interface{})
	if stats == nil {
		t.Fatalf("missing meta.stats: %v", meta)
	}
	if got, _ := stats["accelerated_by"].(string); got != "mv_accel_diff" {
		t.Fatalf("meta.stats.accelerated_by = %q, want %q (meta: %v)", got, "mv_accel_diff", stats)
	}
}
