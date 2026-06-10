package views

import (
	"testing"
)

func TestAnalyzeLynxFlow_CountByHost(t *testing.T) {
	an, err := AnalyzeLynxFlow(`from main | stats count() by host`)
	if err != nil {
		t.Fatalf("AnalyzeLynxFlow: %v", err)
	}
	if an.SourceIndex != "main" {
		t.Errorf("SourceIndex: got %q, want %q", an.SourceIndex, "main")
	}
	if !an.IsAggregation {
		t.Fatal("expected IsAggregation=true")
	}
	if an.AggSpec == nil {
		t.Fatal("AggSpec is nil")
	}
	if len(an.AggSpec.Funcs) != 1 {
		t.Fatalf("AggSpec.Funcs: got %d, want 1", len(an.AggSpec.Funcs))
	}
	if an.AggSpec.Funcs[0].Name != "count" {
		t.Errorf("func name: got %q, want %q", an.AggSpec.Funcs[0].Name, "count")
	}
	if len(an.GroupBy) != 1 || an.GroupBy[0] != "host" {
		t.Errorf("GroupBy: got %v, want [host]", an.GroupBy)
	}
	if an.Plan == nil {
		t.Fatal("Plan is nil")
	}
}

func TestAnalyzeLynxFlow_FilteredAgg(t *testing.T) {
	an, err := AnalyzeLynxFlow(`from main | where status >= 500 | stats count(), avg(duration) by host`)
	if err != nil {
		t.Fatalf("AnalyzeLynxFlow: %v", err)
	}
	if !an.IsAggregation {
		t.Fatal("expected IsAggregation=true")
	}
	if an.StreamingPlan == nil {
		t.Fatal("StreamingPlan is nil (expected pre-agg filter)")
	}
	// count + avg + hidden _mv_auto_count = 3 funcs.
	if len(an.AggSpec.Funcs) < 2 {
		t.Fatalf("AggSpec.Funcs: got %d, want at least 2 (count + avg)", len(an.AggSpec.Funcs))
	}
}

func TestAnalyzeLynxFlow_ProjectionView(t *testing.T) {
	an, err := AnalyzeLynxFlow(`from main | where level == "error"`)
	if err != nil {
		t.Fatalf("AnalyzeLynxFlow: %v", err)
	}
	if an.IsAggregation {
		t.Fatal("expected IsAggregation=false for projection view")
	}
	if an.AggSpec != nil {
		t.Fatal("AggSpec should be nil for projection view")
	}
}

func TestAnalyzeLynxFlow_RejectsUnsupportedAgg(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{"values", `from main | stats values(host)`},
		{"stdev", `from main | stats stdev(duration)`},
		{"percentile", `from main | stats p99(duration)`},
		{"earliest", `from main | stats earliest(msg)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := AnalyzeLynxFlow(tt.query)
			if err == nil {
				t.Fatalf("expected error for unsupported agg %q", tt.name)
			}
		})
	}
}

func TestAnalyzeLynxFlow_RejectsJoin(t *testing.T) {
	// Join requires two sources — should be rejected.
	_, err := AnalyzeLynxFlow(`from main | join type=inner host [from other]`)
	if err == nil {
		t.Fatal("expected error for join in MV")
	}
}

func TestAnalyzeLynxFlow_AggSpecCompatibleWithSPL2(t *testing.T) {
	// The critical compatibility property: LynxFlow and SPL2 produce
	// the same func Name and Field for each agg. The Alias may differ
	// in syntax (count() vs count) but for migrated views the AggSpec
	// is PRESERVED from the original SPL2 definition, so aliases stay
	// identical. Here we verify the semantic compatibility.
	lfAn, err := AnalyzeLynxFlow(`from main | stats count(), sum(bytes), avg(duration) by host`)
	if err != nil {
		t.Fatalf("AnalyzeLynxFlow: %v", err)
	}

	spl2An, err := AnalyzeQuery(`FROM main | stats count, sum(bytes), avg(duration) by host`)
	if err != nil {
		t.Fatalf("AnalyzeQuery: %v", err)
	}

	// Compare func Name and Field (storage-critical). Alias differs but
	// is preserved on migration.
	if len(lfAn.AggSpec.Funcs) != len(spl2An.AggSpec.Funcs) {
		t.Fatalf("Funcs length mismatch: lynxflow=%d, spl2=%d",
			len(lfAn.AggSpec.Funcs), len(spl2An.AggSpec.Funcs))
	}
	for i, lf := range lfAn.AggSpec.Funcs {
		sp := spl2An.AggSpec.Funcs[i]
		if lf.Name != sp.Name {
			t.Errorf("func[%d] name: lynxflow=%q, spl2=%q", i, lf.Name, sp.Name)
		}
		if lf.Field != sp.Field {
			t.Errorf("func[%d] field: lynxflow=%q, spl2=%q", i, lf.Field, sp.Field)
		}
	}

	if len(lfAn.GroupBy) != len(spl2An.GroupBy) {
		t.Fatalf("GroupBy length mismatch: lynxflow=%v, spl2=%v", lfAn.GroupBy, spl2An.GroupBy)
	}
	for i := range lfAn.GroupBy {
		if lfAn.GroupBy[i] != spl2An.GroupBy[i] {
			t.Errorf("GroupBy[%d]: lynxflow=%q, spl2=%q", i, lfAn.GroupBy[i], spl2An.GroupBy[i])
		}
	}
}
