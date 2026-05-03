package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/storage/part"
)

// systemTableResolver implements pipeline.SystemTableResolver using the
// Engine's part registry and compactor to populate virtual system tables.
type systemTableResolver struct {
	engine *Engine
}

// ResolveSystemTable returns rows for the given system table.
// Supported tables: "parts", "columns", "queries", "query_operators".
func (r *systemTableResolver) ResolveSystemTable(ctx context.Context, table string) ([]map[string]event.Value, error) {
	switch table {
	case "parts":
		return r.resolveParts(ctx)
	case "columns":
		return r.resolveColumns(ctx)
	case "queries":
		return r.resolveQueries(ctx)
	case "query_operators":
		return r.resolveQueryOperators(ctx)
	default:
		return nil, fmt.Errorf("unknown system table %q (available: system.parts, system.columns, system.queries, system.query_operators)", table)
	}
}

// resolveParts returns one row per part from the part registry.
// Columns: id, index, partition, level, event_count, size_bytes, min_time, max_time, columns, tier, created_at.
func (r *systemTableResolver) resolveParts(_ context.Context) ([]map[string]event.Value, error) {
	registry := r.engine.partRegistry
	if registry == nil {
		// In-memory mode: build rows from segmentHandle metadata.
		return r.resolvePartsInMemory(), nil
	}

	parts := registry.All()
	rows := make([]map[string]event.Value, 0, len(parts))

	for _, p := range parts {
		rows = append(rows, partToRow(p))
	}

	return rows, nil
}

// resolvePartsInMemory builds system.parts rows from in-memory segments
// when no part registry exists (in-memory mode without a data directory).
func (r *systemTableResolver) resolvePartsInMemory() []map[string]event.Value {
	r.engine.mu.RLock()
	segs := r.engine.currentEpoch.Load().segments
	r.engine.mu.RUnlock()

	rows := make([]map[string]event.Value, 0, len(segs))

	for _, sh := range segs {
		row := map[string]event.Value{
			"id":          event.StringValue(sh.meta.ID),
			"index":       event.StringValue(sh.index),
			"level":       event.IntValue(int64(sh.meta.Level)),
			"event_count": event.IntValue(sh.meta.EventCount),
			"size_bytes":  event.IntValue(sh.meta.SizeBytes),
			"min_time":    event.StringValue(sh.meta.MinTime.Format("2006-01-02T15:04:05Z")),
			"max_time":    event.StringValue(sh.meta.MaxTime.Format("2006-01-02T15:04:05Z")),
			"tier":        event.StringValue("hot"),
		}
		rows = append(rows, row)
	}

	return rows
}

func partToRow(p *part.Meta) map[string]event.Value {
	tier := p.Tier
	if tier == "" {
		tier = "hot"
	}

	cols := strings.Join(p.Columns, ",")

	return map[string]event.Value{
		"id":           event.StringValue(p.ID),
		"index":        event.StringValue(p.Index),
		"partition":    event.StringValue(p.Partition),
		"level":        event.IntValue(int64(p.Level)),
		"event_count":  event.IntValue(p.EventCount),
		"size_bytes":   event.IntValue(p.SizeBytes),
		"min_time":     event.StringValue(p.MinTime.Format("2006-01-02T15:04:05Z")),
		"max_time":     event.StringValue(p.MaxTime.Format("2006-01-02T15:04:05Z")),
		"columns":      event.StringValue(cols),
		"column_count": event.IntValue(int64(len(p.Columns))),
		"tier":         event.StringValue(tier),
		"created_at":   event.StringValue(p.CreatedAt.Format("2006-01-02T15:04:05Z")),
	}
}

// resolveColumns returns one row per distinct column name found across all parts.
// Columns: name, type, part_count, total_events, coverage_pct.
func (r *systemTableResolver) resolveColumns(_ context.Context) ([]map[string]event.Value, error) {
	// Collect column statistics from part registry or in-memory segments.
	type colStats struct {
		partCount   int
		totalEvents int64
	}

	stats := make(map[string]*colStats)
	var totalParts int
	var totalEvents int64

	if r.engine.partRegistry != nil {
		parts := r.engine.partRegistry.All()
		totalParts = len(parts)

		for _, p := range parts {
			totalEvents += p.EventCount
			for _, col := range p.Columns {
				cs, ok := stats[col]
				if !ok {
					cs = &colStats{}
					stats[col] = cs
				}

				cs.partCount++
				cs.totalEvents += p.EventCount
			}
		}
	} else {
		r.engine.mu.RLock()
		segs := r.engine.currentEpoch.Load().segments
		r.engine.mu.RUnlock()

		totalParts = len(segs)

		for _, sh := range segs {
			totalEvents += sh.meta.EventCount
			if sh.reader != nil {
				for _, col := range sh.reader.ColumnNames() {
					cs, ok := stats[col]
					if !ok {
						cs = &colStats{}
						stats[col] = cs
					}

					cs.partCount++
					cs.totalEvents += sh.meta.EventCount
				}
			}
		}
	}

	rows := make([]map[string]event.Value, 0, len(stats))

	for name, cs := range stats {
		coveragePct := float64(0)
		if totalParts > 0 {
			coveragePct = float64(cs.partCount) / float64(totalParts) * 100
		}

		rows = append(rows, map[string]event.Value{
			"name":         event.StringValue(name),
			"part_count":   event.IntValue(int64(cs.partCount)),
			"total_events": event.IntValue(cs.totalEvents),
			"coverage_pct": event.StringValue(strconv.FormatFloat(coveragePct, 'f', 1, 64)),
		})
	}

	return rows, nil
}

func (r *systemTableResolver) resolveQueries(_ context.Context) ([]map[string]event.Value, error) {
	rows := make([]map[string]event.Value, 0)
	r.engine.jobs.Range(func(_, value any) bool {
		job := value.(*SearchJob)
		job.mu.Lock()
		defer job.mu.Unlock()

		var doneAt event.Value
		if !job.DoneAt.IsZero() {
			doneAt = event.TimestampValue(job.DoneAt)
		} else {
			doneAt = event.NullValue()
		}

		var spilledRows int64
		for _, stage := range job.Stats.PipelineStages {
			spilledRows += stage.SpilledRows
		}

		rows = append(rows, map[string]event.Value{
			"id":                event.StringValue(job.ID),
			"query":             event.StringValue(job.Query),
			"status":            event.StringValue(job.Status),
			"created_at":        event.TimestampValue(job.CreatedAt),
			"done_at":           doneAt,
			"peak_memory_bytes": event.IntValue(job.Stats.PeakMemoryBytes),
			"spilled_to_disk":   event.BoolValue(job.Stats.SpilledToDisk),
			"spilled_rows":      event.IntValue(spilledRows),
			"spill_bytes":       event.IntValue(job.Stats.SpillBytes),
			"rows_scanned":      event.IntValue(job.Stats.RowsScanned),
			"rows_returned":     event.IntValue(job.Stats.RowsReturned),
			"error_type":        event.StringValue(job.Stats.ErrorType),
		})

		return true
	})

	return rows, nil
}

func (r *systemTableResolver) resolveQueryOperators(_ context.Context) ([]map[string]event.Value, error) {
	rows := make([]map[string]event.Value, 0)
	r.engine.jobs.Range(func(_, value any) bool {
		job := value.(*SearchJob)
		job.mu.Lock()
		defer job.mu.Unlock()

		for _, stage := range job.Stats.PipelineStages {
			rows = append(rows, map[string]event.Value{
				"query_id":     event.StringValue(job.ID),
				"operator":     event.StringValue(stage.Name),
				"input_rows":   event.IntValue(stage.InputRows),
				"output_rows":  event.IntValue(stage.OutputRows),
				"duration_ms":  event.FloatValue(stage.DurationMS),
				"exclusive_ms": event.FloatValue(stage.ExclusiveMS),
				"memory_bytes": event.IntValue(stage.MemoryBytes),
				"spilled_rows": event.IntValue(stage.SpilledRows),
				"spill_bytes":  event.IntValue(stage.SpillBytes),
			})
		}

		for _, budget := range job.Stats.OperatorBudgets {
			rows = append(rows, map[string]event.Value{
				"query_id":     event.StringValue(job.ID),
				"operator":     event.StringValue(budget.Label),
				"soft_limit":   event.IntValue(budget.SoftLimit),
				"peak_bytes":   event.IntValue(budget.PeakBytes),
				"spilled":      event.BoolValue(budget.Spilled),
				"budget_phase": event.StringValue(budget.Phase),
			})
		}

		return true
	})

	return rows, nil
}
