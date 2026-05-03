//go:build e2e

package shippers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/client"
)

func waitForSourceCount(t *testing.T, rig *TestRig, source string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	query := fmt.Sprintf(`FROM main | where _source=%q | STATS count AS total`, source)

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var last int
	for ctx.Err() == nil {
		result, err := rig.Client.QuerySync(ctx, query, "", "")
		if err == nil {
			last = intFromResult(result, "total")
			if last >= want {
				return
			}
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	t.Fatalf("source %q count = %d, want >= %d", source, last, want)
}

func intFromResult(result *client.QueryResult, field string) int {
	if result == nil || result.Aggregate == nil || len(result.Aggregate.Rows) == 0 {
		return 0
	}
	idx := -1
	for i, col := range result.Aggregate.Columns {
		if col == field {
			idx = i
			break
		}
	}
	if idx < 0 || idx >= len(result.Aggregate.Rows[0]) {
		return 0
	}
	switch v := result.Aggregate.Rows[0][idx].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return 0
	}
}
