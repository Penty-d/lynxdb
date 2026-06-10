// Package logical defines the typed logical plan IR for LynxFlow v2 queries
// (RFC-002 Phase 4a).
//
// The logical plan is a tree of [Node] values produced by lowering a desugared
// LynxFlow AST via [Lower]. Each node represents a relational algebra operator
// (Scan, Filter, Project, Aggregate, ...) and knows its input child(ren) and
// output schema.
//
// # Design decisions
//
// Expressions stay as [ast.Expr]. The VM compiles directly from AST expression
// nodes, so introducing a separate expression IR would create a translation
// layer with no benefit at this stage. If expression-level optimisation
// (constant folding, CSE) is added later, an expr IR can be inserted between
// ast.Expr and the VM without changing the plan node interfaces.
//
// Schema propagation reuses [sema.Field] / [sema.FieldType] directly. The
// sema package already defines the canonical flowing-schema types and the
// inference logic for every operator. Duplicating that type system would
// create drift risk with no upside. Plan nodes delegate to sema-style logic
// for schema computation; the initial schema comes from the provided catalog
// (or is left open when no catalog is available).
//
// Time bounds are stored as RELATIVE markers (the original duration text and
// snap text from the AST), not resolved to wall-clock time. This makes the
// plan deterministic and cacheable regardless of when it was built.
//
// The tree is linear-with-branches: most nodes have exactly one input child
// (accessed via Children/SetChildren). [Join] has a left child (in Children)
// plus a separate Right field. [Union] has N children. [Scan] has zero
// children.
//
// Pushdown is an empty struct in this PR; fields land in PR (c).
//
// # Fusion rules applied during lowering
//
//   - sort immediately followed by head fuses into TopK
//   - consecutive keep/drop/rename stages fuse into a single Project
//   - bin(_time, d) in a stats by-list is extracted into TimeBin and removed
//     from Keys
package logical
