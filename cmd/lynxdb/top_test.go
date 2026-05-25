package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	zone "github.com/lrstanley/bubblezone/v2"

	"github.com/lynxbase/lynxdb/internal/ui"
	"github.com/lynxbase/lynxdb/pkg/client"
)

func TestTopRenderWidths(t *testing.T) {
	resetAllFlags(t)
	zone.NewGlobal()
	defer zone.Close()

	for _, width := range []int{60, 100, 140} {
		m := sampleTopModel(width, 32)
		view := m.View()
		if view.MouseMode != tea.MouseModeCellMotion {
			t.Fatalf("width %d mouse mode = %v", width, view.MouseMode)
		}
		for i, line := range strings.Split(view.Content, "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("width %d line %d overflow: got %d: %q", width, i+1, got, line)
			}
		}
	}
}

func TestTopRenderStates(t *testing.T) {
	resetAllFlags(t)
	zone.NewGlobal()
	defer zone.Close()

	m := sampleTopModel(100, 28)
	m.paused = true
	m.err = assertErr("connection refused with a long diagnostic that should not overflow the top header")
	m.filtering = true
	m.filterInput = "slow query"
	m.help = true

	view := m.View().Content
	for _, want := range []string{"paused", "slow query", "Active Queries", "q quit"} {
		if !strings.Contains(view, want) {
			t.Fatalf("render missing %q in %q", want, view)
		}
	}
}

func TestTopSortAndFilterRows(t *testing.T) {
	resetAllFlags(t)
	m := sampleTopModel(100, 28)
	m.filter = "api"

	rows := m.sortedFilteredRows()
	if len(rows) != 2 {
		t.Fatalf("filtered rows: got %d, want 2", len(rows))
	}

	m.sortMode = topSortMemory
	rows = m.sortedFilteredRows()
	if rows[0].JobID != "qry_api_highmem" {
		t.Fatalf("memory sort first row = %s", rows[0].JobID)
	}

	m.sortMode = topSortSpill
	rows = m.sortedFilteredRows()
	if rows[0].JobID != "qry_api_spill" {
		t.Fatalf("spill sort first row = %s", rows[0].JobID)
	}
}

func TestTopActiveQueryDetailActionsAndStatus(t *testing.T) {
	resetAllFlags(t)
	zone.NewGlobal()
	defer zone.Close()

	m := sampleTopModel(140, 34)
	m.expandedJobID = "qry_api_highmem"
	m.detailMode = "detail"

	view := m.View().Content
	for _, want := range []string{
		"STATUS",
		"[detail]",
		"[copy]",
		"[profile]",
		"[cancel]",
		"query: FROM api | where level=\"error\" | stats count by host",
		"progress:",
		"resources:",
		"done",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("render missing %q in %q", want, view)
		}
	}
}

func TestTopProfileCommandQuotesQuery(t *testing.T) {
	got := profileCommandForQuery("FROM api | where msg='oops'")
	want := "lynxdb query --analyze full 'FROM api | where msg='\\''oops'\\'''"
	if got != want {
		t.Fatalf("profile command = %q, want %q", got, want)
	}
}

func TestTopMemoryPanelShowsGovernorClasses(t *testing.T) {
	resetAllFlags(t)
	zone.NewGlobal()
	defer zone.Close()

	m := sampleTopModel(120, 32)
	view := m.View().Content
	if !strings.Contains(view, "page-cache") {
		t.Fatalf("render missing page-cache memory class in %q", view)
	}
}

func sampleTopModel(width, height int) topModel {
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	m := topModel{
		theme:     ui.Stdout,
		server:    "http://localhost:3100",
		interval:  2 * time.Second,
		loaded:    true,
		lastLoad:  now,
		width:     width,
		height:    height,
		histories: make(map[string][]float64),
		snapshot: &client.TopSnapshot{
			Server: client.TopServerSnapshot{
				Version:       "test",
				Health:        "healthy",
				UptimeSeconds: 3661,
				DataDir:       "(in-memory)",
				Profile:       "ephemeral",
			},
			Events: client.TopEventsSnapshot{
				Total:         123456,
				Today:         1234,
				Buffered:      12,
				IngestRateEPS: 42,
			},
			Storage: client.TopStorageSnapshot{
				UsedBytes:       12 << 20,
				SegmentCount:    8,
				SegmentBytes:    12 << 20,
				SegmentsByLevel: map[string]int{"L0": 4, "L1": 2, "L2": 1, "L3": 1},
				OldestEvent:     now.Add(-2 * time.Hour).Format(time.RFC3339),
			},
			Indexes: []client.TopIndexSnapshot{
				{Name: "api", EventCount: 100000, SegmentCount: 4, SizeBytes: 10 << 20, ActiveQueries: 2},
				{Name: "main", EventCount: 23456, SegmentCount: 4, SizeBytes: 2 << 20},
			},
			Queries: client.TopQueriesSnapshot{
				Active:       2,
				Recent:       3,
				CacheHitRate: 0.75,
				Rows: []client.TopQueryRow{
					{
						JobID:              "qry_api_highmem",
						Query:              "FROM api | where level=\"error\" | stats count by host",
						Status:             "running",
						CreatedAt:          now.Add(-10 * time.Second),
						ElapsedMS:          10000,
						Phase:              "scanning_segments",
						Percent:            30,
						RowsReadSoFar:      5000,
						SegmentsTotal:      10,
						SegmentsScanned:    3,
						CurrentMemoryBytes: 8 << 20,
						Indexes:            []string{"api"},
					},
					{
						JobID:           "qry_api_spill",
						Query:           "FROM api | sort duration desc",
						Status:          "running",
						CreatedAt:       now.Add(-20 * time.Second),
						ElapsedMS:       20000,
						Phase:           "executing_pipeline",
						Percent:         80,
						RowsReadSoFar:   9000,
						SegmentsTotal:   10,
						SegmentsScanned: 8,
						SpillBytes:      16 << 20,
						SpillFiles:      2,
						Indexes:         []string{"api"},
					},
					{
						JobID:           "qry_main_done",
						Query:           "FROM main | head 10",
						Status:          "done",
						CreatedAt:       now.Add(-30 * time.Second),
						ElapsedMS:       3000,
						Percent:         100,
						RowsReadSoFar:   10,
						SegmentsTotal:   2,
						SegmentsScanned: 2,
						Indexes:         []string{"main"},
					},
				},
			},
			Memory: client.TopMemorySnapshot{
				Governor: &client.TopGovernorStats{
					Allocated: 32 << 20,
					Limit:     128 << 20,
					ByClass: []client.TopClassStats{
						{},
						{},
						{},
						{Allocated: 4 << 20},
					},
				},
				SpillFiles: 2,
				SpillBytes: 16 << 20,
			},
			Cluster: client.TopClusterSnapshot{
				Status:         "healthy",
				NodeCount:      1,
				IndexCount:     2,
				SegmentCount:   8,
				BufferedEvents: 12,
				DataDir:        "(in-memory)",
			},
		},
	}
	m.updateHistories()
	return m
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
