// Package query implements distributed query execution for LynxDB clusters.
// It provides scatter-gather coordination, shard pruning, partial aggregation
// merge, and distributed live tail.
package query

import (
	"fmt"
	"strings"

	"github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/spl2"
)

// MergeStrategy describes how shard results are combined by the coordinator.
type MergeStrategy int

const (
	// MergeConcat streams rows from all shards and applies coordinator commands.
	MergeConcat MergeStrategy = iota
	// MergePartialAgg uses two-phase aggregation: partial on shards, merge on coordinator.
	MergePartialAgg
	// MergeTopK combines partial aggregation with heap-based top-K selection.
	MergeTopK
)

// String returns a human-readable name for the merge strategy.
func (s MergeStrategy) String() string {
	switch s {
	case MergeConcat:
		return "concat"
	case MergePartialAgg:
		return "partial_agg"
	case MergeTopK:
		return "topk"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// DistributedPlan describes how a query is split between shards and coordinator.
type DistributedPlan struct {
	// ShardQuery is the SPL2 text to execute on each shard.
	ShardQuery string
	// ShardCommands are the commands pushed to shards.
	ShardCommands []spl2.Command
	// CoordCommands are the commands executed on the coordinator after merge.
	CoordCommands []spl2.Command
	// Strategy determines how shard results are combined.
	Strategy MergeStrategy
	// PartialAggSpec describes the partial aggregation (non-nil for MergePartialAgg/MergeTopK).
	PartialAggSpec *pipeline.PartialAggSpec
	// TopK is the number of top results to keep (only for MergeTopK).
	TopK int
	// TopKSortFields are the sort fields for TopK selection.
	TopKSortFields []pipeline.SortField
	// SplitIndex is the position in the original command list where the split occurs.
	SplitIndex int
}

// PlanDistributedQuery splits an SPL2 program into shard-level and coordinator-level
// commands. It walks commands forward, accumulating pushable commands into the shard
// prefix. On the first non-pushable command, it splits. If the split point is a
// StatsCommand, it extracts a PartialAggSpec for two-phase aggregation.
func PlanDistributedQuery(prog *spl2.Program) (*DistributedPlan, error) {
	if prog == nil || prog.Main == nil || len(prog.Main.Commands) == 0 {
		return &DistributedPlan{Strategy: MergeConcat}, nil
	}

	commands := prog.Main.Commands
	splitIdx := findSplitPoint(commands)

	plan := &DistributedPlan{
		SplitIndex: splitIdx,
	}

	if splitIdx == 0 {
		// No pushable commands — stream all rows and run full pipeline on coordinator.
		plan.Strategy = MergeConcat
		plan.CoordCommands = commands
		plan.ShardQuery = buildShardQueryText(prog, nil)

		return plan, nil
	}

	plan.ShardCommands = commands[:splitIdx]
	plan.CoordCommands = commands[splitIdx:]

	// Check if the last shard command is StatsCommand — use partial aggregation.
	lastShard := plan.ShardCommands[len(plan.ShardCommands)-1]
	if statsCmd, ok := lastShard.(*spl2.StatsCommand); ok {
		spec := extractPartialAggSpecFromStats(statsCmd)
		if spec != nil && allPushable(spec) {
			plan.PartialAggSpec = spec
			plan.Strategy = MergePartialAgg

			// Check for TopK pattern: stats + sort + head
			if topK, sortFields := detectTopK(plan.CoordCommands); topK > 0 {
				plan.Strategy = MergeTopK
				plan.TopK = topK
				plan.TopKSortFields = sortFields
				// Remove the sort+head from coord commands since TopK merge handles them.
				plan.CoordCommands = plan.CoordCommands[2:]
			}
		} else {
			// Stats has non-pushable aggs — fall back to concat.
			plan.Strategy = MergeConcat
		}
	} else if topCmd, ok := lastShard.(*spl2.TopCommand); ok {
		// Top N is a special case of TopK with implicit stats count.
		plan.PartialAggSpec = &pipeline.PartialAggSpec{
			GroupBy: topGroupBy(topCmd),
			Funcs:   []pipeline.PartialAggFunc{{Name: "count", Alias: "count"}},
		}
		plan.Strategy = MergeTopK
		plan.TopK = topCmd.N
		plan.TopKSortFields = []pipeline.SortField{{Name: "count", Desc: true}}
	} else if rareCmd, ok := lastShard.(*spl2.RareCommand); ok {
		plan.PartialAggSpec = &pipeline.PartialAggSpec{
			GroupBy: rareGroupBy(rareCmd),
			Funcs:   []pipeline.PartialAggFunc{{Name: "count", Alias: "count"}},
		}
		plan.Strategy = MergeTopK
		plan.TopK = rareCmd.N
		plan.TopKSortFields = []pipeline.SortField{{Name: "count", Desc: false}}
	} else {
		plan.Strategy = MergeConcat
	}

	plan.ShardQuery = buildShardQueryText(prog, plan.ShardCommands)

	return plan, nil
}

// findSplitPoint walks commands forward and returns the index of the first
// non-pushable command. If all commands are pushable, returns len(commands).
func findSplitPoint(commands []spl2.Command) int {
	for i, cmd := range commands {
		if !isPushable(cmd) {
			return i
		}
	}

	return len(commands)
}

// isPushable returns true if a command can be executed on individual shards.
func isPushable(cmd spl2.Command) bool {
	switch cmd := cmd.(type) {
	case *spl2.SearchCommand,
		*spl2.WhereCommand,
		*spl2.EvalCommand,
		*spl2.RexCommand,
		*spl2.BinCommand,
		*spl2.FillnullCommand,
		*spl2.RenameCommand,
		*spl2.StatsCommand,
		*spl2.TopCommand,
		*spl2.RareCommand:
		return true
	case *spl2.FieldsCommand:
		return !cmd.Remove // only keep-mode is safe to push
	default:
		return false
	}
}

// allPushable checks that all aggregation functions in the spec support
// partial aggregation (decomposable into partial + merge).
func allPushable(spec *pipeline.PartialAggSpec) bool {
	for _, f := range spec.Funcs {
		if !pipeline.IsPushableAgg(f.Name) {
			return false
		}
	}

	return true
}

// extractPartialAggSpecFromStats builds a PartialAggSpec from a StatsCommand.
func extractPartialAggSpecFromStats(cmd *spl2.StatsCommand) *pipeline.PartialAggSpec {
	spec := &pipeline.PartialAggSpec{
		GroupBy: cmd.GroupBy,
		Funcs:   make([]pipeline.PartialAggFunc, len(cmd.Aggregations)),
	}

	for i, agg := range cmd.Aggregations {
		field := ""
		if len(agg.Args) > 0 {
			if fe, ok := agg.Args[0].(*spl2.FieldExpr); ok {
				field = fe.Name
			}
		}

		alias := agg.Alias
		if alias == "" {
			if field != "" {
				alias = fmt.Sprintf("%s(%s)", strings.ToLower(agg.Func), field)
			} else {
				alias = strings.ToLower(agg.Func)
			}
		}

		spec.Funcs[i] = pipeline.PartialAggFunc{
			Name:  strings.ToLower(agg.Func),
			Field: field,
			Alias: alias,
		}
	}

	return spec
}

// detectTopK checks if the coordinator commands start with sort + head,
// which can be optimized to TopK merge.
func detectTopK(coordCommands []spl2.Command) (int, []pipeline.SortField) {
	if len(coordCommands) < 2 {
		return 0, nil
	}

	sortCmd, ok := coordCommands[0].(*spl2.SortCommand)
	if !ok {
		return 0, nil
	}

	headCmd, ok := coordCommands[1].(*spl2.HeadCommand)
	if !ok {
		return 0, nil
	}

	sortFields := make([]pipeline.SortField, len(sortCmd.Fields))
	for i, sf := range sortCmd.Fields {
		sortFields[i] = pipeline.SortField{Name: sf.Name, Desc: sf.Desc}
	}

	return headCmd.Count, sortFields
}

// topGroupBy returns the group-by fields for a TopCommand.
func topGroupBy(cmd *spl2.TopCommand) []string {
	fields := []string{cmd.Field}
	if cmd.ByField != "" {
		fields = append(fields, cmd.ByField)
	}

	return fields
}

// rareGroupBy returns the group-by fields for a RareCommand.
func rareGroupBy(cmd *spl2.RareCommand) []string {
	fields := []string{cmd.Field}
	if cmd.ByField != "" {
		fields = append(fields, cmd.ByField)
	}

	return fields
}

// buildShardQueryText reconstructs SPL2 text from a program and shard commands.
// If shardCommands is nil, returns just the source clause for full scan.
func buildShardQueryText(prog *spl2.Program, shardCommands []spl2.Command) string {
	var sb strings.Builder

	// Add source clause if present.
	if prog.Main.Source != nil {
		sb.WriteString("FROM ")
		if prog.Main.Source.IsGlob {
			sb.WriteString(prog.Main.Source.Index)
		} else if len(prog.Main.Source.Indices) > 0 {
			sb.WriteString(strings.Join(prog.Main.Source.Indices, ", "))
		} else {
			sb.WriteString(prog.Main.Source.Index)
		}
	}

	for _, cmd := range shardCommands {
		sb.WriteString(" | ")
		sb.WriteString(cmd.String())
	}

	return sb.String()
}
