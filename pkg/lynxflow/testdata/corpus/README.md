# LynxFlow v2 acceptance corpus

Hand-translated query pairs pinning the RFC-002 migration semantics (Phase 0
deliverable; PLAN.md §18.1). Each line of `corpus.jsonl` is one entry:

| Field | Meaning |
|---|---|
| `id` | stable key (`c001`...), referenced by golden AST and golden error files in later phases |
| `name` | short slug |
| `source` | provenance: CLI golden transcript, sigma golden, or docs file |
| `spl2` | the original RFC-001 SPL2/LynxFlow query, verbatim |
| `lynxflow` | the hand-translated LynxFlow v2 query per `docs/grammar/RFC-002.md` |
| `features` | language features exercised (for coverage accounting) |
| `notes` | semantic deltas a reviewer or the differential harness must know about |

Coverage: search sugar (bare terms, phrases, key:value, globs), one-grammar
expressions (in/between/not/parens), conditional aggregates, percentile aliases,
extend/keep/drop/rename, dedup/sort/head/tail, every/bin time bucketing, xyseries,
eventstats/streamstats, CTEs (`let $x`), union, join, transaction/compare/describe
helpers, kept sugar (top/rare/latency/rate/percentiles), killed sugar with written-out
expansions (errors/slowest/fillnull/f-strings), parse + explode + object access,
materialize, and all 10 distinct sigma rule shapes.

Consumers:

- **Phase 2 gate**: every `lynxflow` value must parse; golden AST dumps are keyed by
  `id`. The go/no-go gate (PLAN.md §19) requires this corpus green with golden ASTs
  and golden error messages.
- **Phase 4+**: the differential harness runs `spl2` on the old runtime and
  `lynxflow` on the new one against identical data and asserts result equality,
  modulo the documented deltas in `notes`.
- **Phase 9**: the rsigma emitter and the MV/saved-query translator are validated
  against the sigma and aggregation entries.
