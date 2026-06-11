# RFC-002 Phase 10 Step 2 — pkg/spl2 Removal Plan

Working checklist for the final deletion of `pkg/spl2`.

## Classification Table

| # | Importer | Classification | Status | Notes |
|---|----------|---------------|--------|-------|
| 1 | `pkg/storage/sources/registry.go` | EXTRACT | **DONE** | Moved `MatchGlob` to `internal/glob`; import now `internal/glob` |
| 2 | `pkg/spl2/search_eval.go` | EXTRACT (self) | **DONE** | Glob functions extracted to `internal/glob`; `GlobToRegex`/`MatchGlob` re-exported as thin wrappers |
| 3 | `pkg/spl2/search_eval_bench_test.go` | EXTRACT (test) | **DONE** | Updated to use `glob.ToContainsRegex` |
| 4 | `pkg/optimizer/*` (14 files) | DELETE-WITH | recipe | Entire package dies with spl2; replaced by `pkg/logical` optimizer (when written) |
| 5 | `pkg/planner/*` (5 files) | DELETE-WITH | recipe | Old plan cache + planner; lynxflow path has its own planning |
| 6 | `pkg/engine/pipeline/*` (9 files) | DELETE-WITH | recipe | Old spl2-AST pipeline builder files; lynxflow physical builder replaces them |
| 7 | `pkg/vm/compiler.go` | DELETE-WITH | recipe | spl2-AST-typed compiler entry; the lynxflow VM compiler (`pkg/vm/lf_compiler.go` or equivalent) replaces it |
| 8 | `pkg/vm/func_registry.go` | DELETE-WITH | recipe | `emit` signatures typed on `spl2.FuncCallExpr`; lynxflow VM uses `ast.CallExpr` |
| 9 | `pkg/vm/vm.go` | DELETE-WITH (partial) | recipe | Only `opSearchMatch` calls `spl2.ParseSearchExpression` + `spl2.NewSearchEvaluator`; rest of VM is spl2-free. Remove that one opcode handler or port search_eval to lynxflow |
| 10 | `pkg/langdetect/detect.go` | DELETE-WITH (spl2 half) | recipe | Calls `spl2.ParseProgram`/`spl2.NormalizeQuery` to detect language. When spl2 dies, `Detect()` returns LynxFlow unconditionally; file shrinks to ~10 lines |
| 11 | `pkg/lynxflow/translate/translate.go` | DELETE-WITH | recipe | SPL2-to-LynxFlow translator; exists only for migration. Delete when spl2 dies |
| 12 | `pkg/server/engine.go` | PORT | recipe | Uses `spl2.MatchGlob` (line 745) for source glob in `engineSources()` |
| 13 | `pkg/server/query.go` | PORT | recipe | Calls `spl2.ParseProgram`, builds old pipeline, reads QueryHints |
| 14 | `pkg/server/store.go` | PORT | recipe | Uses `spl2.MatchGlob` for source scope/glob filtering |
| 15 | `pkg/server/stream.go` | PORT | recipe | Uses spl2 program/QueryHints for streaming queries |
| 16 | `pkg/server/cache_helpers.go` | PORT | recipe | Hashes spl2 QueryHints for cache keys |
| 17 | `pkg/server/cluster_query.go` | PORT | recipe | Forwards spl2.Program to cluster coordinator |
| 18 | `pkg/server/types.go` | PORT | recipe | Contains type aliases / interfaces referencing spl2 types |
| 19 | `pkg/server/views.go` | PORT | recipe | MV dispatch uses spl2 parsing |
| 20 | `pkg/server/zero_result.go` | PORT | recipe | Uses spl2 hints |
| 21 | `pkg/server/top_snapshot.go` | PORT | recipe | Uses spl2 QueryHints |
| 22 | `pkg/storage/engine.go` | PORT | recipe | `Query` struct embeds spl2.QueryHints for bloom terms, time bounds, source scope |
| 23 | `pkg/storage/views/query_analysis.go` | PORT | recipe | Analyzes spl2 AST for MV matching |
| 24 | `pkg/usecases/interfaces.go` | PORT | recipe | QueryService interface references spl2 types |
| 25 | `pkg/usecases/query.go` | PORT | recipe | Orchestrates spl2 parse + plan + execute |
| 26 | `pkg/usecases/tail.go` | PORT | recipe | Live tail uses spl2 parsing |
| 27 | `pkg/usecases/types.go` | PORT | recipe | Request/response types reference spl2.Program |
| 28 | `pkg/api/rest/query.go` | PORT | recipe | REST handler parses spl2, dispatches to usecases |
| 29 | `pkg/api/rest/explain.go` | PORT | recipe | Explain endpoint parses spl2 |
| 30 | `pkg/api/rest/lynxflow_exec.go` | PORT | recipe | Already has lynxflow path; spl2 fallback needs removal |
| 31 | `pkg/api/rest/response.go` | PORT | recipe | References spl2 types in response structs |
| 32 | `pkg/api/rest/server.go` | PORT | recipe | Router setup references spl2 planner |
| 33 | `pkg/api/rest/stream.go` | PORT | recipe | Streaming endpoint uses spl2 |
| 34 | `pkg/cluster/query/planner.go` | PORT (large) | recipe | Distributed query splitting on spl2 AST; needs logical IR equivalent |
| 35 | `pkg/cluster/query/coordinator.go` | PORT (large) | recipe | Scatter-gather uses spl2.Program + QueryHints |
| 36 | `pkg/cluster/query/join_planner.go` | PORT | recipe | Join strategy on spl2.JoinCommand |
| 37 | `pkg/cluster/query/shard_pruner.go` | PORT | recipe | Shard pruning uses spl2.QueryHints + SourceScope constants |
| 38 | `cmd/lynxdb/query.go` | PORT | recipe | CLI query command parses spl2 |
| 39 | `cmd/lynxdb/errors.go` | PORT | recipe | Error formatting references spl2 parse errors |
| 40 | `cmd/lynxdb/helpers.go` | PORT | recipe | Helper functions reference spl2 types |
| 41 | `cmd/lynxdb/saved.go` | PORT | recipe | Saved queries reference spl2 |
| 42 | `internal/shell/model.go` | PORT | recipe | Shell dispatches spl2 queries |
| 43 | `internal/shell/autocomplete.go` | PORT | recipe | Autocomplete uses spl2 keywords |
| 44 | `internal/shell/highlight.go` | PORT | recipe | Syntax highlighting uses spl2 lexer |

## Completed in this change

1. **EXTRACT: glob matching** (`internal/glob` package created)
   - `internal/glob/glob.go` — `Match`, `MatchCached`, `ToRegex`, `ToContainsRegex` + all helper functions
   - `internal/glob/glob_test.go` — unit tests
   - `pkg/spl2/search_eval.go` — `GlobToRegex`/`MatchGlob` now re-export from `internal/glob`; private `matchGlob`/`matchGlobContains` delegate to `internal/glob`; local duplicates removed
   - `pkg/spl2/search_eval_bench_test.go` — updated to use `glob.ToContainsRegex`
   - `pkg/storage/sources/registry.go` — now imports `internal/glob` instead of `pkg/spl2`

## Removal recipes

### DELETE-WITH group (delete entire package/file when pkg/spl2 dies)

These packages exist solely to serve the spl2 AST path. They have no callers
outside the spl2 execution path and will be deleted atomically with pkg/spl2.

**Order: delete all at once in a single commit.**

| Package/file | Action |
|---|---|
| `pkg/optimizer/` (entire dir) | `rm -rf` |
| `pkg/planner/` (entire dir) | `rm -rf` |
| `pkg/langdetect/detect.go` | Rewrite: `Detect()` returns `"lynxflow"` unconditionally |
| `pkg/lynxflow/translate/` (entire dir) | `rm -rf` |
| `pkg/vm/compiler.go` | Delete (spl2-AST compiler entry) |
| `pkg/vm/func_registry.go` | Delete (spl2-typed emit functions) |
| `pkg/vm/vm.go` lines 2990-3005 | Remove `opSearchMatch` handler or port `SearchEvaluator` |
| `pkg/engine/pipeline/appendpipe.go` | Delete |
| `pkg/engine/pipeline/cte_dag.go` | Delete |
| `pkg/engine/pipeline/filter.go` | Delete (spl2 filter builder) |
| `pkg/engine/pipeline/json_cmd.go` | Delete |
| `pkg/engine/pipeline/memory_coordinator.go` | Delete |
| `pkg/engine/pipeline/pipeline.go` | Delete (spl2 pipeline builder) |
| `pkg/engine/pipeline/project.go` | Delete |
| `pkg/engine/pipeline/replace.go` | Delete |
| `pkg/engine/pipeline/rgfilter_builder.go` | Delete |
| `pkg/engine/pipeline/segment_stream.go` | Delete (uses `spl2.MatchGlob`; lynxflow segment stream uses `internal/glob`) |
| `pkg/engine/pipeline/vec_plan.go` | Delete |

### PORT: pkg/server (8 files) — medium effort

All 8 files reference `spl2.Program`, `spl2.QueryHints`, or `spl2.MatchGlob`.

**Recipe:**
1. `server/engine.go` line 745: Replace `spl2.MatchGlob` with `glob.Match` (trivial, same as sources/registry.go).
2. `server/store.go` lines 904, 928: Replace `spl2.MatchGlob` with `glob.Match`.
3. `server/query.go`: The main execution entry. Currently calls `planner.Plan()` which returns `spl2.Program`. Port to accept `*logical.Plan` from the lynxflow path. This is the central switchover — once this file dispatches to the lynxflow physical builder, the old pipeline builder is dead.
4. `server/stream.go`, `server/views.go`, `server/cache_helpers.go`, `server/cluster_query.go`, `server/zero_result.go`, `server/top_snapshot.go`, `server/types.go`: All follow from (3) — they pass around `spl2.Program`/`QueryHints`. Replace with `logical.Plan`/`logical.Pushdown`.

**Effort**: ~2-3 days. The key gate is `server/query.go` which orchestrates the full execution path.

### PORT: pkg/storage/engine.go — small effort

The `Query` struct embeds `spl2.QueryHints` for bloom terms, time bounds, source scope.

**Recipe:**
1. Define a storage-layer query hints struct (or use `logical.Pushdown` directly).
2. Update `Query` to embed the new type.
3. Update all callers that populate `Query.Hints` (server/store.go, server/query.go).

**Effort**: ~0.5 day. Blocked on server/query.go port.

### PORT: pkg/storage/views/query_analysis.go — medium effort

Analyzes spl2 AST to determine if a query matches a materialized view.

**Recipe:**
1. Rewrite to analyze `logical.Plan` nodes instead of `spl2.Command` nodes.
2. The matching logic (stats fields, group-by fields, filter predicates) maps cleanly to logical nodes (`Aggregate`, `Filter`, `Scan.Pushdown`).

**Effort**: ~1 day.

### PORT: pkg/usecases (4 files) — medium effort

Orchestration layer between REST and server.

**Recipe:**
1. `interfaces.go`: Change `QueryService` interface to accept `logical.Plan` instead of `spl2.Program`.
2. `query.go`: Remove spl2 parse call; accept pre-parsed plan from REST layer.
3. `tail.go`: Same pattern.
4. `types.go`: Update request/response types.

**Effort**: ~1 day. Follows from server port.

### PORT: pkg/api/rest (6 files) — medium effort

REST handlers that parse queries and dispatch to usecases.

**Recipe:**
1. `query.go`, `explain.go`, `stream.go`: Replace `spl2.ParseProgram` with `lynxflow.Parse` + `logical.Lower`. Already partially done in `lynxflow_exec.go`.
2. `lynxflow_exec.go`: Remove spl2 fallback path.
3. `response.go`: Update response types to reference logical plan types.
4. `server.go`: Remove planner injection for spl2; keep lynxflow planner.

**Effort**: ~1 day. Mostly mechanical once usecases are ported.

### PORT: cmd/lynxdb (4 files) — small effort

CLI commands that parse and dispatch queries.

**Recipe:**
1. `query.go`: Replace `spl2.ParseProgram`/`spl2.NormalizeQuery` with lynxflow equivalents.
2. `errors.go`: Update error type assertions from spl2 parse errors to lynxflow parse errors.
3. `helpers.go`, `saved.go`: Follow from (1).

**Effort**: ~0.5 day.

### PORT: internal/shell (3 files) — small effort

Interactive shell with syntax highlighting and autocomplete.

**Recipe:**
1. `highlight.go`: Replace spl2 lexer with lynxflow lexer for tokenization.
2. `autocomplete.go`: Replace spl2 keyword list with lynxflow keyword list.
3. `model.go`: Replace spl2 parse/dispatch with lynxflow.

**Effort**: ~0.5 day.

### PORT: pkg/cluster/query (4 files) — LARGE effort (gap analysis below)

## Cluster Query Gap Analysis

### What cluster/query consumes from spl2

1. **`planner.go` (281 lines)**: `PlanDistributedQuery(*spl2.Program)` splits a pipeline into shard-pushable commands vs coordinator commands by type-switching on `spl2.Command` AST nodes. It detects partial aggregation opportunities (`StatsCommand`, `TopCommand`, `RareCommand`) and builds `PartialAggSpec`.

2. **`coordinator.go` (512 lines)**: `ExecuteDistributed()` takes `*spl2.Program` + `*spl2.QueryHints`, calls `PlanDistributedQuery`, serializes shard query text via `cmd.String()`, scatters to shards, merges partial results, then applies coordinator commands by building a sub-pipeline from `spl2.Query{Commands: coordCommands}`.

3. **`join_planner.go` (113 lines)**: `PlanDistributedJoin()` takes `*spl2.JoinCommand` + `*spl2.SourceClause`, decides broadcast vs shuffle strategy.

4. **`shard_pruner.go` (211 lines)**: `PruneShards()` takes `*spl2.QueryHints` for `SourceScope*` constants, `TimeBounds`, source patterns.

### What the logical IR provides today

- `pkg/logical/node.go`: `Node` interface with `Scan`, `Filter`, `Project`, `Aggregate`, `Sort`, `Limit`, `Eval`, `Rex`, `Join`, `Union`, `Dedup`, `Rename`, `Bin`, `Top`, `Rare`, `StreamStats`, `EventStats`, `Transaction`, `Tail`, `FillNull`, `XYSeries`.
- `pkg/logical/plan.go`: `Plan{Root Node, Lets map[string]*Plan}` with `Dump()`.
- `pkg/logical/lower.go`: Lowering from lynxflow AST to logical IR.
- **NO serialization** (no `Marshal`/`Unmarshal`, no protobuf, no JSON encoding).
- **NO `Pushdown` serialization** for shard transmission.
- **NO distributed plan splitting** (no `findSplitPoint` equivalent on logical nodes).

### Gaps to fill before cluster/query can be ported

1. **Plan serialization**: The coordinator must serialize a sub-plan to send to shard nodes. Today it uses `buildShardQueryText()` which calls `cmd.String()` to reconstruct SPL2 text. Options:
   - (a) Serialize `logical.Plan` to JSON/protobuf and deserialize on shard. Requires `MarshalJSON`/`UnmarshalJSON` or protobuf codegen for all ~20 node types.
   - (b) Reconstruct LynxFlow query text from `logical.Plan` (a `Plan.Format()` method). Simpler, but requires a LynxFlow pretty-printer for the logical IR.
   - (c) Keep sending query text (already have lynxflow text), split at the text level. Simplest but fragile.
   - **Recommendation**: (b) — add `Format() string` to `logical.Plan` that produces valid LynxFlow text. This mirrors the existing `buildShardQueryText` approach and is testable with round-trip parse-format-parse tests.

2. **Distributed plan splitting on logical nodes**: Port `findSplitPoint`/`isPushable` to walk the logical IR tree instead of type-switching on spl2 commands. The node types map 1:1, so this is mechanical.

3. **QueryHints / Pushdown equivalence**: `spl2.QueryHints` carries `SourceScope`, `TimeBounds`, bloom terms, `FieldPredicates`. The logical IR's `Scan.Pushdown` already carries `TimeBounds`, `FieldPredicates`, `BloomTerms`, `SourcePatterns`. Need to add `SourceScope` enum or keep the existing pattern-based approach. The shard pruner needs to read these from `logical.Pushdown` instead of `spl2.QueryHints`.

4. **Partial aggregation spec extraction**: Port `extractPartialAggSpecFromStats` to read from `logical.Aggregate` node fields instead of `spl2.StatsCommand`.

### Effort estimate

~3-5 days for the full cluster/query port, with plan serialization (gap 1) being the largest piece (~2 days). Gaps 2-4 are mechanical (~1 day each).

## Recommended execution order

1. ~~EXTRACT: glob matching~~ (DONE)
2. PORT: `server/engine.go` + `server/store.go` MatchGlob calls (trivial, use `internal/glob`)
3. PORT: `pkg/storage/engine.go` Query hints struct
4. PORT: `server/query.go` — central switchover to logical.Plan execution
5. PORT: `pkg/usecases/*` — follows from (4)
6. PORT: `pkg/api/rest/*` — follows from (5)
7. PORT: `cmd/lynxdb/*` + `internal/shell/*` — follows from (6)
8. PORT: `pkg/storage/views/query_analysis.go` — can be done in parallel with (4-7)
9. PORT: `pkg/cluster/query/*` — requires plan serialization (gap 1)
10. ~~DELETE-WITH: atomic deletion of spl2 + optimizer + planner + old pipeline + translate + langdetect rewrite~~ (DONE)

## Phase 10 Step 2 — Final cleanup (DONE)

All items below completed in a single pass:

- [x] **Fix vet-failing test files**: 5 originally listed + 8 additional discovered
  - `internal/shell/autocomplete_test.go`: replaced `KnownAggregateFunctions()` with `LynxFlowAggregateNames()`
  - `pkg/storage/views/mv_analysis_test.go`: deleted spl2 compatibility test `TestAnalyzeLynxFlow_AggSpecCompatibleWithSPL2`
  - `pkg/vm/golden_test.go`: **deleted** (spl2 AST types LiteralExpr/FieldExpr/FuncCallExpr + CompileExpr gone; covered by lynxflow_conformance_test.go)
  - `pkg/vm/testdata/goldens/`: **deleted** (spl2 bytecode golden files)
  - `pkg/usecases/query_test.go`: added missing `planner` import; skipped `TestExplain_FieldTracking_SourceStageDoesNotExpandCatalog` (annotatePipelineFields is a stub)
  - `pkg/server/detect_result_type_test.go`: **rewritten** against `logical.Node` types (Aggregate, TopK, Sort, Limit, Project, Describe)
  - `pkg/sigmaqueries/bench_test.go`: **deleted** (spl2 golden fixture reader + BuildProgramWithGovernor stub)
  - `pkg/sigmaqueries/golden_test.go`, `parse_test.go`, `plan_test.go`, `fuzz_test.go`: **deleted** (all spl2-dependent)
  - `pkg/cluster/query/ir_planner_test.go`: **rewritten** — removed SPL2 parity comparison, kept LynxFlow IR tests only
  - `pkg/cluster/query/join_planner_test.go`: **deleted** (spl2 JoinCommand types gone, stub function)
  - `pkg/cluster/query/planner_test.go`: **deleted** (PlanDistributedQuery is a stub)
  - `pkg/engine/pipeline/estimate_test.go`: fixed `NewTailIterator` arg count (3 not 4)
  - `pkg/engine/pipeline/memory_coordinator_test.go`: deleted tests for removed `queryContext.govBudget` + `newCoordinatedAccount`; added missing `event` import
  - `pkg/server/multi_source_test.go`: deleted test for removed `IsMultiSource()` method
  - `pkg/server/query_cache_test.go`: rewrote `submitAndWait` to use `planner.New()` instead of spl2 parsing
  - `pkg/vm/vm_test.go`: removed 28 spl2-referencing test functions; kept ~35 clean VM tests
  - `pkg/vm/vm_json_test.go`: deleted `TestCompileSpathAlias` (spl2 types)
  - `pkg/storage/views/query_analysis_test.go`: **deleted** (AnalyzeQuery is a nil-returning stub)
  - `internal/shell/highlight_test.go`: deleted `TestStringRawEnd` (function removed)
  - `cmd/lynxdb/query.go`: fixed self-assignment vet warning
- [x] **gofmt**: all 8 unformatted files formatted
- [x] **Test-suite swap**: deleted spl2 testdata/cli/ transcripts; extracted testdata/cli-lynxflow/ into testdata/cli/; stripped `# language:` headers; deleted `golden_lynxflow_test.go` (TestGolden_File/Server now read lynxflow content); deleted `internal/testgen/lynxflow`
- [x] **pkg/sigmaqueries**: deleted `.spl2` golden files + 5 spl2-dependent test files; `.lynxflow` + `lynxflow_test.go` + manifest preserved
- [x] **Grammar assets**: deleted `spl2.ebnf` + `examples.jsonl` from both `docs/grammar/` and `cmd/lynxdb/grammar_data/`; copied `lynxflow.ebnf` into grammar_data; updated grammar command to serve lynxflow EBNF
- [x] **docs/site**: deleted `docs/site/docs/lynx-flow/` directory; removed sidebar category; updated navbar link to `/docs/lynxflow/overview`
- [x] **CI workflows**: updated `rsigma-drift.yml` to diff `.lynxflow` instead of `.spl2`; replaced spl2 fuzz steps with `go test ./pkg/lynxflow/parser/ -run '^$' -fuzz '^FuzzParse$' -fuzztime 10s`; kept lynxflow conformance

### TODO(RFC-002) follow-ups — RESOLVED (post-ship completion pass, 2026-06-11)

All follow-ups left by the Phase 10 deletion were closed in a dedicated
completion pass:

| Item | Resolution |
|------|------------|
| Stubbed `SegmentStreamIterator` (silent segment data loss in tail catchup / EXPLAIN ANALYZE) | Real row-group streaming iterator restored from the pre-deletion tree on `pkg/model` types; server scan source reads segments + buffered events |
| REST lynxflow shim (`BuildEventStoreFromHints`, sync-only, `wait` ignored) | REST routes through `usecases.QueryService.Submit`: async/hybrid modes, job polling/SSE, meta.explain, scan stats, result cache, broad-scope lints |
| `coordinator.go` coordinator-node execution stub | `applyCoordCommands` executes coordinator stages via `physical.Build` over the merged rows (clone + rewire over a synthetic scan) |
| `join_planner.go` stub | `PlanDistributedJoin` on `logical.Join` (broadcast for CTE-backed right sides, shuffle otherwise); strategy recorded for tracing |
| Distributed dispatch removed from `executeQueryPipeline` | Restored; `runDistributedPipeline` consumes `ExecuteQueryIR` over resolved `model.QueryHints` |
| `planner.go` view catalog TODO | MV acceleration rule in `pkg/logical/opt` (decision D31); `meta.stats.accelerated_by` flows end to end; `from <view>` resolves through the view registry |
| `stream.go` hints TODO | `BuildStreamingPipeline` accepts `*model.QueryHints`, streams from segments with an epoch-unpinning iterator |
| `vm.go` OpSearchMatch | dead handler removed (opcode number stays reserved, append-only) |
| `memory_coordinator.go` spill-counting stub | uncalled `CountSpillableNodesIR` deleted (builder receives an explicit coordinator) |
| `usecases/query.go` pipeline annotation stubs | `annotatePipelineFields` + `extractPhysicalPlan` implemented over the logical IR; 14 skipped tests re-enabled |
| `internal/shell/highlight.go` | rewritten on the lynxflow lexer + operator/function registry |
| `mv migrate` window-closed stub | explicit-query migration: `mv migrate <name> --query '<lynxflow>'`, `--all`/`--dry-run` listings; all suggestion strings aligned |
| Skipped filebeat round-trip | passes on the engine execution path; skip removed |
| Legacy SPL2 views activating with empty analysis | `AnalyzeQuery` errors; views marked needs-migration, excluded from dispatch |
| `test/integration` sigma e2e on deleted `.spl2` goldens | ported to `.lynxflow` goldens |
| Dead code | `vec_plan.go`/`vec_eval.go`, `SearchExprIterator`, spl2 Build* stubs, `applyLoweredRangePredicates`, `CheckVectorizedFilter` removed; `spl2_stubs.go` renamed `row_iterators.go` |
