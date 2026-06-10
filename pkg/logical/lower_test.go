package logical

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/desugar"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
)

// parseDesugarLower parses a full query, desugars, and lowers it.
func parseDesugarLower(t *testing.T, query string) (*Plan, []Diag) {
	t.Helper()
	q, pDiags := parser.Parse(query)
	if len(pDiags) > 0 {
		t.Fatalf("parse error in %q: %v", query, formatDiags(pDiags))
	}
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	return Lower(desugared, Options{DefaultSource: "main"})
}

func formatDiags(ds []parser.Diag) string {
	var parts []string
	for _, d := range ds {
		parts = append(parts, fmt.Sprintf("[%s] %s", d.Code, d.Message))
	}
	return strings.Join(parts, "; ")
}

// Corpus golden plan tests have been moved to pkg/logical/opt/opt_test.go
// (TestCorpus_OptimizedGoldenPlans) so that golden plans show the optimized
// plan (expression simplification + plan-level rules). The import cycle
// (logical -> opt -> logical) prevents importing opt from this package's
// internal tests.

// ---------------------------------------------------------------------------
// Fusion tests
// ---------------------------------------------------------------------------

func TestFusion_SortHead_TopK(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | sort -count | head 10`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*TopK](t, plan.Root, "root should be TopK")
	topk := plan.Root.(*TopK)
	if topk.K != 10 {
		t.Errorf("TopK.K = %d, want 10", topk.K)
	}
	if len(topk.SortKeys) != 1 || !topk.SortKeys[0].Desc {
		t.Errorf("TopK.SortKeys mismatch: %v", topk.SortKeys)
	}
}

func TestFusion_SortHead_NotFused_WhenInterleaved(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | sort -count | where status == 200 | head 10`)
	assertNoDiagErrors(t, diags)
	// Should be Limit(head 10) with Filter in between, not TopK.
	assertNodeType[*Limit](t, plan.Root, "root should be Limit, not TopK")
}

func TestFusion_KeepDropRename_SingleProject(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | keep host, status | rename host as hostname | drop status`)
	assertNoDiagErrors(t, diags)
	// All three should fuse into a single Project.
	assertNodeType[*Project](t, plan.Root, "root should be Project")
	proj := plan.Root.(*Project)
	if len(proj.Cols) != 4 {
		t.Errorf("Project.Cols = %d, want 4 (keep host, keep status, rename host->hostname, drop status)", len(proj.Cols))
	}
}

func TestFusion_KeepRename_Consecutive(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | keep host, status | rename host as h`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*Project](t, plan.Root, "root should be Project")
	proj := plan.Root.(*Project)
	if len(proj.Cols) != 3 {
		t.Errorf("Project.Cols = %d, want 3", len(proj.Cols))
	}
}

func TestFusion_DropAlone_NoFusion(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | drop host`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*Project](t, plan.Root, "root should be Project")
}

func TestFusion_Stats_BinExtracted(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | stats count() by service, bin(_time, 5m)`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*Aggregate](t, plan.Root, "root should be Aggregate")
	agg := plan.Root.(*Aggregate)
	if agg.TimeBin == nil {
		t.Fatal("TimeBin should be extracted from bin(_time, 5m)")
	}
	if len(agg.Keys) != 1 || agg.Keys[0].Name != "service" {
		t.Errorf("Keys should be [service], got %v", agg.Keys)
	}
}

func TestFusion_Stats_NoBin_NoTimeBin(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | stats count() by service`)
	assertNoDiagErrors(t, diags)
	agg := plan.Root.(*Aggregate)
	if agg.TimeBin != nil {
		t.Fatal("TimeBin should be nil when no bin() in by-list")
	}
}

func TestFusion_TopSugar_SortHeadTopK(t *testing.T) {
	// top desugars to stats+sort+head; sort+head fuses to TopK.
	plan, diags := parseDesugarLower(t,
		`from app | top 5 service`)
	assertNoDiagErrors(t, diags)
	// Root should be TopK.
	assertNodeType[*TopK](t, plan.Root, "root should be TopK (fused from desugared top)")
}

// ---------------------------------------------------------------------------
// CTE tests
// ---------------------------------------------------------------------------

func TestCTE_LetAndFromRef(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`let $a = from app | where level == "ERROR"; from $a | stats count()`)
	assertNoDiagErrors(t, diags)

	if len(plan.Lets) != 1 {
		t.Fatalf("Lets: want 1, got %d", len(plan.Lets))
	}
	aPlan, ok := plan.Lets["a"]
	if !ok {
		t.Fatal("missing CTE $a in Lets")
	}
	// The CTE root should be a Filter.
	assertNodeType[*Filter](t, aPlan.Root, "CTE $a root should be Filter")

	// Main pipeline root should be Aggregate with the CTE's Filter as input.
	assertNodeType[*Aggregate](t, plan.Root, "main root should be Aggregate")
	agg := plan.Root.(*Aggregate)
	// The input to Aggregate should be a shared pointer to the CTE root.
	if agg.Input != aPlan.Root {
		t.Error("Aggregate input should be a shared pointer to $a's root")
	}
}

func TestCTE_JoinWithCTERef(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`let $b = from audit | stats count() as failures by src_ip; from app | join on client_ip with $b`)
	assertNoDiagErrors(t, diags)

	bPlan, ok := plan.Lets["b"]
	if !ok {
		t.Fatal("missing CTE $b in Lets")
	}

	assertNodeType[*Join](t, plan.Root, "main root should be Join")
	join := plan.Root.(*Join)
	if join.Right != bPlan.Root {
		t.Error("Join.Right should be a shared pointer to $b's root")
	}
}

// ---------------------------------------------------------------------------
// Round-trip safety: Lower is pure (input AST unchanged)
// ---------------------------------------------------------------------------

func TestLower_ASTUnchanged(t *testing.T) {
	query := `from app[-1h] timeout status>=500 | where level == "ERROR" | stats count() by service, bin(_time, 5m) | sort -count | head 10`
	q, pDiags := parser.Parse(query)
	if len(pDiags) > 0 {
		t.Fatalf("parse error: %v", formatDiags(pDiags))
	}
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})

	dumpBefore := ast.Dump(desugared)

	_, _ = Lower(desugared, Options{DefaultSource: "main"})

	dumpAfter := ast.Dump(desugared)
	if dumpBefore != dumpAfter {
		t.Errorf("Lower mutated the AST:\n--- before ---\n%s\n--- after ---\n%s", dumpBefore, dumpAfter)
	}
}

// ---------------------------------------------------------------------------
// Additional edge cases
// ---------------------------------------------------------------------------

func TestLower_NilQuery(t *testing.T) {
	plan, diags := Lower(nil, Options{})
	if plan == nil {
		t.Fatal("plan should not be nil for nil input")
	}
	if len(diags) != 0 {
		t.Errorf("expected no diags, got %d", len(diags))
	}
}

func TestLower_ImplicitSource(t *testing.T) {
	plan, diags := parseDesugarLower(t, `| stats count()`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*Aggregate](t, plan.Root, "root should be Aggregate")
}

func TestLower_Eventstats(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | eventstats sum(bytes) as total_bytes by service`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*Aggregate](t, plan.Root, "root should be Aggregate")
	agg := plan.Root.(*Aggregate)
	if agg.Window == nil || agg.Window.Variant != WindowEventstats {
		t.Error("expected WindowEventstats variant")
	}
}

func TestLower_Streamstats(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | streamstats window=10 sum(bytes) as running_bytes`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*Aggregate](t, plan.Root, "root should be Aggregate")
	agg := plan.Root.(*Aggregate)
	if agg.Window == nil || agg.Window.Variant != WindowStreamstats {
		t.Error("expected WindowStreamstats variant")
	}
	if agg.Window.Window == nil || *agg.Window.Window != 10 {
		t.Errorf("expected window=10, got %v", agg.Window.Window)
	}
}

func TestLower_Parse(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | parse json into (status as int, user as string) on_error drop`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*Parse](t, plan.Root, "root should be Parse")
	p := plan.Root.(*Parse)
	if p.Format != "json" {
		t.Errorf("Format = %q, want json", p.Format)
	}
	if p.OnError != "drop" {
		t.Errorf("OnError = %q, want drop", p.OnError)
	}
	if len(p.Captures) != 2 {
		t.Errorf("Captures = %d, want 2", len(p.Captures))
	}
}

func TestLower_Explode(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | explode tags as tag`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*Explode](t, plan.Root, "root should be Explode")
	e := plan.Root.(*Explode)
	if e.Field != "tags" || e.As != "tag" {
		t.Errorf("Explode: field=%q as=%q", e.Field, e.As)
	}
}

func TestLower_Describe(t *testing.T) {
	plan, diags := parseDesugarLower(t, `from app | describe`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*Describe](t, plan.Root, "root should be Describe")
}

func TestLower_Tail(t *testing.T) {
	plan, diags := parseDesugarLower(t, `from app | tail 20`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*Limit](t, plan.Root, "root should be Limit")
	lim := plan.Root.(*Limit)
	if !lim.Tail {
		t.Error("Limit.Tail should be true")
	}
	if lim.N != 20 {
		t.Errorf("Limit.N = %d, want 20", lim.N)
	}
}

func TestLower_Materialize(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | stats count() by service | materialize "mv_test" retention=90d`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*Materialize](t, plan.Root, "root should be Materialize")
	m := plan.Root.(*Materialize)
	if m.Name != "mv_test" {
		t.Errorf("Name = %q, want mv_test", m.Name)
	}
}

func TestLower_Tee(t *testing.T) {
	plan, diags := parseDesugarLower(t,
		`from app | tee "debug_sink"`)
	assertNoDiagErrors(t, diags)
	assertNodeType[*Tee](t, plan.Root, "root should be Tee")
}

// ---------------------------------------------------------------------------
// Plan.Dump basic sanity
// ---------------------------------------------------------------------------

func TestDump_BasicPipeline(t *testing.T) {
	plan, _ := parseDesugarLower(t,
		`from app | where status >= 500 | head 10`)
	dump := plan.Dump()
	if !strings.Contains(dump, "Filter(") {
		t.Error("dump should contain Filter")
	}
	if !strings.Contains(dump, "Limit(") {
		t.Error("dump should contain Limit")
	}
	if !strings.Contains(dump, "Scan(") {
		t.Error("dump should contain Scan")
	}
}

// ---------------------------------------------------------------------------
// Schema propagation
// ---------------------------------------------------------------------------

func TestSchema_StatsReplacesSchema(t *testing.T) {
	plan, _ := parseDesugarLower(t,
		`from app | stats count() as n by service`)
	schema := plan.Root.Schema()
	if len(schema) != 2 {
		t.Fatalf("schema should have 2 fields, got %d: %v", len(schema), schema)
	}
	// service, n
	names := make([]string, len(schema))
	for i, f := range schema {
		names[i] = f.Name
	}
	if names[0] != "service" || names[1] != "n" {
		t.Errorf("schema field names = %v, want [service, n]", names)
	}
}

func TestSchema_FilterPassesThrough(t *testing.T) {
	plan, _ := parseDesugarLower(t,
		`from app | where level == "ERROR"`)
	schema := plan.Root.Schema()
	if len(schema) != 6 {
		t.Errorf("schema should have 6 builtin fields, got %d", len(schema))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func assertNoDiagErrors(t *testing.T, diags []Diag) {
	t.Helper()
	var errors []string
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			errors = append(errors, fmt.Sprintf("[%s] %s", d.Code, d.Message))
		}
	}
	if len(errors) > 0 {
		t.Fatalf("unexpected error diags:\n  %s", strings.Join(errors, "\n  "))
	}
}

func assertNodeType[T Node](t *testing.T, n Node, msg string) {
	t.Helper()
	if _, ok := n.(T); !ok {
		t.Fatalf("%s: got %T, want %T", msg, n, *new(T))
	}
}
