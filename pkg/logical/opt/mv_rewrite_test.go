package opt

import (
	"testing"

	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/format"
)

// stubCatalog implements ViewCatalog for tests.
type stubCatalog struct {
	views []ViewInfo
}

func (s *stubCatalog) ListViewInfos() []ViewInfo { return s.views }

// buildTestPlan constructs a simple Scan(main) -> Filter(expr) -> Aggregate(aggs, keys) plan.
func buildTestPlan(filterExpr ast.Expr, aggs []logical.Agg, keys []logical.Key) *logical.Plan {
	scan := &logical.Scan{
		Sources: []logical.SourcePattern{
			{Kind: ast.SourceName, Name: "main"},
		},
	}

	var root logical.Node = scan

	if filterExpr != nil {
		f := &logical.Filter{Expr: filterExpr}
		f.SetChildren([]logical.Node{root})
		root = f
	}

	if len(aggs) > 0 || len(keys) > 0 {
		agg := &logical.Aggregate{Aggs: aggs, Keys: keys}
		agg.SetChildren([]logical.Node{root})
		root = agg
	}

	return &logical.Plan{Root: root}
}

func levelEqError() ast.Expr {
	return &ast.Binary{
		Op:    ast.OpEq,
		Left:  &ast.Ident{Name: "level"},
		Right: &ast.Literal{Kind: ast.LitString, Raw: `"error"`, Value: "error"},
	}
}

func countAgg(alias string) logical.Agg {
	return logical.Agg{
		Func:  &ast.Call{Callee: "count", Args: nil},
		Alias: alias,
	}
}

func sumAgg(field, alias string) logical.Agg {
	return logical.Agg{
		Func:  &ast.Call{Callee: "sum", Args: []ast.Expr{&ast.Ident{Name: field}}},
		Alias: alias,
	}
}

func avgAgg(field, alias string) logical.Agg {
	return logical.Agg{
		Func:  &ast.Call{Callee: "avg", Args: []ast.Expr{&ast.Ident{Name: field}}},
		Alias: alias,
	}
}

func serviceKey() logical.Key {
	return logical.Key{Name: "service", Expr: &ast.Ident{Name: "service"}}
}

func hostKey() logical.Key {
	return logical.Key{Name: "host", Expr: &ast.Ident{Name: "host"}}
}

func TestMVRewrite_ExactMatch(t *testing.T) {
	// View: from main | where level == "error" | stats count() as cnt by service
	viewFilter := format.Expr(levelEqError())

	catalog := &stubCatalog{
		views: []ViewInfo{{
			Name:    "mv_errors",
			Status:  "active",
			Source:  "main",
			Filter:  viewFilter,
			GroupBy: []string{"service"},
			Aggs: []AggInfo{
				{Func: "count", Arg: "", Alias: "cnt"},
			},
			RowCount: 100,
		}},
	}

	// Query: from main | where level == "error" | stats count() as cnt by service
	plan := buildTestPlan(
		levelEqError(),
		[]logical.Agg{countAgg("cnt")},
		[]logical.Key{serviceKey()},
	)

	_, applied, accel := OptimizeWithViews(plan, Options{Views: catalog})

	if accel == nil {
		t.Fatal("expected MV acceleration, got nil")
	}
	if accel.ViewName != "mv_errors" {
		t.Errorf("expected ViewName=mv_errors, got %s", accel.ViewName)
	}
	if accel.Status != "active" {
		t.Errorf("expected Status=active, got %s", accel.Status)
	}

	// Verify the mv-rewrite rule fired.
	found := false
	for _, a := range applied {
		if a.Rule == "mv-rewrite" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected mv-rewrite rule in applied list")
	}

	// The rewritten plan should scan the view.
	var scanNode *logical.Scan
	walkPlan(plan.Root, func(n logical.Node) bool {
		if s, ok := n.(*logical.Scan); ok {
			scanNode = s
		}
		return false
	})
	if scanNode == nil {
		t.Fatal("no Scan node in rewritten plan")
	}
	if len(scanNode.Sources) != 1 || scanNode.Sources[0].Name != "mv_errors" {
		t.Errorf("expected Scan(mv_errors), got %v", scanNode.Sources)
	}
}

func TestMVRewrite_SubsetGroupBy_Rollup(t *testing.T) {
	// View: stats count() as cnt, sum(bytes) as total_bytes by service, host
	viewFilter := format.Expr(levelEqError())

	catalog := &stubCatalog{
		views: []ViewInfo{{
			Name:    "mv_detail",
			Status:  "active",
			Source:  "main",
			Filter:  viewFilter,
			GroupBy: []string{"service", "host"},
			Aggs: []AggInfo{
				{Func: "count", Arg: "", Alias: "cnt"},
				{Func: "sum", Arg: "bytes", Alias: "total_bytes"},
			},
			RowCount: 1000,
		}},
	}

	// Query: stats count() as cnt by service (subset group-by, count -> sum(cnt))
	plan := buildTestPlan(
		levelEqError(),
		[]logical.Agg{countAgg("cnt")},
		[]logical.Key{serviceKey()},
	)

	_, _, accel := OptimizeWithViews(plan, Options{Views: catalog})

	if accel == nil {
		t.Fatal("expected MV acceleration for subset group-by, got nil")
	}
	if accel.ViewName != "mv_detail" {
		t.Errorf("expected ViewName=mv_detail, got %s", accel.ViewName)
	}
}

func TestMVRewrite_SubsetGroupBy_AvgRefused(t *testing.T) {
	// View has count and avg; query asks for avg with subset group-by.
	// avg is NOT correctly derivable from finalized rows -> REFUSE.
	viewFilter := format.Expr(levelEqError())

	catalog := &stubCatalog{
		views: []ViewInfo{{
			Name:    "mv_avg",
			Status:  "active",
			Source:  "main",
			Filter:  viewFilter,
			GroupBy: []string{"service", "host"},
			Aggs: []AggInfo{
				{Func: "avg", Arg: "duration", Alias: "avg_dur"},
			},
			RowCount: 1000,
		}},
	}

	// Query: stats avg(duration) as avg_dur by service (subset group-by)
	plan := buildTestPlan(
		levelEqError(),
		[]logical.Agg{avgAgg("duration", "avg_dur")},
		[]logical.Key{serviceKey()},
	)

	_, _, accel := OptimizeWithViews(plan, Options{Views: catalog})

	if accel != nil {
		t.Fatal("expected avg with subset group-by to be REFUSED, got acceleration")
	}
}

func TestMVRewrite_FilterMismatch(t *testing.T) {
	// View has filter level == "error"; query has level == "warn".
	viewFilter := format.Expr(levelEqError())

	catalog := &stubCatalog{
		views: []ViewInfo{{
			Name:    "mv_errors",
			Status:  "active",
			Source:  "main",
			Filter:  viewFilter,
			GroupBy: []string{"service"},
			Aggs:    []AggInfo{{Func: "count", Arg: "", Alias: "cnt"}},
		}},
	}

	warnFilter := &ast.Binary{
		Op:    ast.OpEq,
		Left:  &ast.Ident{Name: "level"},
		Right: &ast.Literal{Kind: ast.LitString, Raw: `"warn"`, Value: "warn"},
	}

	plan := buildTestPlan(
		warnFilter,
		[]logical.Agg{countAgg("cnt")},
		[]logical.Key{serviceKey()},
	)

	_, _, accel := OptimizeWithViews(plan, Options{Views: catalog})

	if accel != nil {
		t.Fatal("expected filter mismatch to refuse rewrite")
	}
}

func TestMVRewrite_InactiveViewRefused(t *testing.T) {
	viewFilter := format.Expr(levelEqError())

	catalog := &stubCatalog{
		views: []ViewInfo{{
			Name:    "mv_paused",
			Status:  "paused",
			Source:  "main",
			Filter:  viewFilter,
			GroupBy: []string{"service"},
			Aggs:    []AggInfo{{Func: "count", Arg: "", Alias: "cnt"}},
		}},
	}

	plan := buildTestPlan(
		levelEqError(),
		[]logical.Agg{countAgg("cnt")},
		[]logical.Key{serviceKey()},
	)

	_, _, accel := OptimizeWithViews(plan, Options{Views: catalog})

	if accel != nil {
		t.Fatal("expected paused view to be refused")
	}
}

func TestMVRewrite_NoCatalog(t *testing.T) {
	plan := buildTestPlan(
		levelEqError(),
		[]logical.Agg{countAgg("cnt")},
		[]logical.Key{serviceKey()},
	)

	_, _, accel := OptimizeWithViews(plan, Options{Views: nil})

	if accel != nil {
		t.Fatal("expected no acceleration without catalog")
	}
}

func TestMVRewrite_BackfillingViewAllowed(t *testing.T) {
	viewFilter := format.Expr(levelEqError())

	catalog := &stubCatalog{
		views: []ViewInfo{{
			Name:    "mv_backfill",
			Status:  "backfill",
			Source:  "main",
			Filter:  viewFilter,
			GroupBy: []string{"service"},
			Aggs:    []AggInfo{{Func: "count", Arg: "", Alias: "cnt"}},
		}},
	}

	plan := buildTestPlan(
		levelEqError(),
		[]logical.Agg{countAgg("cnt")},
		[]logical.Key{serviceKey()},
	)

	_, _, accel := OptimizeWithViews(plan, Options{Views: catalog})

	if accel == nil {
		t.Fatal("expected backfilling view to be accepted (partial results)")
	}
	if accel.Status != "backfill" {
		t.Errorf("expected Status=backfill, got %s", accel.Status)
	}
}
