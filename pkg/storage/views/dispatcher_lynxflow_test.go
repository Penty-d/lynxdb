package views

import (
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/event"
)

// createLynxFlowAggView creates a LynxFlow aggregation view via AnalyzeLynxFlow.
func createLynxFlowAggView(t *testing.T, reg *ViewRegistry, name, query string) ViewDefinition {
	t.Helper()

	mvAn, err := AnalyzeLynxFlow(query)
	if err != nil {
		t.Fatalf("AnalyzeLynxFlow: %v", err)
	}

	def := ViewDefinition{
		Name:            name,
		Version:         1,
		Type:            ViewTypeAggregation,
		Query:           query,
		SourceIndex:     mvAn.SourceIndex,
		AggSpec:         mvAn.AggSpec,
		GroupBy:         mvAn.GroupBy,
		LanguageVersion: "lynxflow",
		Columns: []ColumnDef{
			{Name: "_time", Type: event.FieldTypeTimestamp},
		},
		Status:    ViewStatusActive,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := reg.Create(def); err != nil {
		t.Fatalf("Create view %s: %v", name, err)
	}

	return def
}

func TestDispatcher_LynxFlow_CountByHost(t *testing.T) {
	d, reg, _ := setupDispatcher(t)
	def := createLynxFlowAggView(t, reg, "mv_lf_count", `from main | stats count() by host`)
	d.ActivateView(def)

	events := []*event.Event{
		makeTestEventWithHost("main", "web1"),
		makeTestEventWithHost("main", "web2"),
		makeTestEventWithHost("main", "web1"),
		makeTestEventWithHost("main", "web1"),
	}
	if err := d.Dispatch(events); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// ViewAllEvents should finalize: web1=3, web2=1.
	all, err := d.ViewAllEvents("mv_lf_count")
	if err != nil {
		t.Fatalf("ViewAllEvents: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(all))
	}

	byHost := make(map[string]int64)
	for _, ev := range all {
		host := ev.GetField("host").String()
		count := ev.GetField("count()").AsInt()
		byHost[host] = count
	}
	if byHost["web1"] != 3 {
		t.Errorf("web1 count: got %d, want 3", byHost["web1"])
	}
	if byHost["web2"] != 1 {
		t.Errorf("web2 count: got %d, want 1", byHost["web2"])
	}
}

func TestDispatcher_LynxFlow_FilteredAgg(t *testing.T) {
	d, reg, _ := setupDispatcher(t)
	def := createLynxFlowAggView(t, reg, "mv_lf_filter",
		`from main | where status == "500" | stats count() by host`)
	d.ActivateView(def)

	events := []*event.Event{
		makeTestEvent("nginx", "/api/a", "200"),
		makeTestEvent("nginx", "/api/b", "500"),
		makeTestEvent("nginx", "/api/c", "500"),
	}
	// Set host and index on events.
	for _, e := range events {
		e.Index = "main"
		e.SetField("host", event.StringValue("web1"))
	}

	if err := d.Dispatch(events); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	all, err := d.ViewAllEvents("mv_lf_filter")
	if err != nil {
		t.Fatalf("ViewAllEvents: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 group, got %d", len(all))
	}

	count := all[0].GetField("count()").AsInt()
	if count != 2 {
		t.Errorf("count: got %d, want 2 (only status=500 events)", count)
	}
}

func TestDispatcher_LynxFlow_AvgMerge(t *testing.T) {
	// Verify weighted average merge across batches.
	d, reg, _ := setupDispatcher(t)
	def := createLynxFlowAggView(t, reg, "mv_lf_avg",
		`from main | stats avg(duration) by host`)
	d.ActivateView(def)

	// Batch 1: host=web1, durations [10, 20] → sum=30, count=2.
	batch1 := []*event.Event{
		makeTestEventWithDuration("main", "web1", 10),
		makeTestEventWithDuration("main", "web1", 20),
	}
	d.Dispatch(batch1)
	d.FlushView("mv_lf_avg")

	// Batch 2: host=web1, durations [30] → sum=30, count=1.
	batch2 := []*event.Event{
		makeTestEventWithDuration("main", "web1", 30),
	}
	d.Dispatch(batch2)

	// Correct avg = (10+20+30)/3 = 20.0 (NOT (15+30)/2 = 22.5).
	all, err := d.ViewAllEvents("mv_lf_avg")
	if err != nil {
		t.Fatalf("ViewAllEvents: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 group, got %d", len(all))
	}

	avgVal := all[0].GetField("avg(duration)")
	if avgVal.IsNull() {
		t.Fatal("avg(duration) is null")
	}
	got := avgVal.AsFloat()
	if got != 20.0 {
		t.Errorf("avg(duration): got %f, want 20.0 (weighted)", got)
	}
}

func TestDispatcher_LynxFlow_SourceFilter(t *testing.T) {
	d, reg, _ := setupDispatcher(t)
	def := createLynxFlowAggView(t, reg, "mv_lf_src",
		`from nginx | stats count() by status`)
	d.ActivateView(def)

	events := []*event.Event{
		makeTestEventWithIndex("nginx", "200"),
		makeTestEventWithIndex("nginx", "500"),
		makeTestEventWithIndex("api", "200"),   // wrong index
		makeTestEventWithIndex("other", "500"), // wrong index
	}
	d.Dispatch(events)

	all, err := d.ViewAllEvents("mv_lf_src")
	if err != nil {
		t.Fatalf("ViewAllEvents: %v", err)
	}
	totalCount := int64(0)
	for _, ev := range all {
		totalCount += ev.GetField("count()").AsInt()
	}
	if totalCount != 2 {
		t.Errorf("total count: got %d, want 2 (only nginx events)", totalCount)
	}
}

func TestDispatcher_LynxFlow_FlushAndRead(t *testing.T) {
	d, reg, _ := setupDispatcher(t)
	def := createLynxFlowAggView(t, reg, "mv_lf_flush",
		`from main | stats count() by host`)
	d.ActivateView(def)

	// Batch 1: flush to disk.
	batch1 := []*event.Event{
		makeTestEventWithHost("main", "web1"),
		makeTestEventWithHost("main", "web1"),
	}
	d.Dispatch(batch1)
	if err := d.FlushView("mv_lf_flush"); err != nil {
		t.Fatalf("FlushView: %v", err)
	}

	// Batch 2: stays in memory.
	batch2 := []*event.Event{
		makeTestEventWithHost("main", "web1"),
	}
	d.Dispatch(batch2)

	// ViewAllEvents merges disk + memory.
	all, err := d.ViewAllEvents("mv_lf_flush")
	if err != nil {
		t.Fatalf("ViewAllEvents: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 group, got %d", len(all))
	}
	if all[0].GetField("count()").AsInt() != 3 {
		t.Errorf("count: got %d, want 3", all[0].GetField("count()").AsInt())
	}
}

func TestDispatcher_MigratedView_Continuity(t *testing.T) {
	// CRITICAL TEST: A view created with SPL2, then migrated to LynxFlow,
	// must continue serving its existing materialized parts. New data
	// written by the LynxFlow path must merge correctly with old data.
	d, reg, _ := setupDispatcher(t)

	// Phase 1: Create an SPL2 view and ingest data.
	spl2Def := createAggView(t, reg, "mv_migrate", `FROM main | stats count by host`)
	d.ActivateView(spl2Def)

	batch1 := []*event.Event{
		makeTestEventWithHost("main", "web1"),
		makeTestEventWithHost("main", "web1"),
		makeTestEventWithHost("main", "web2"),
	}
	d.Dispatch(batch1)
	if err := d.FlushView("mv_migrate"); err != nil {
		t.Fatalf("FlushView (SPL2 phase): %v", err)
	}

	// Verify SPL2 data is correct before migration.
	pre, err := d.ViewAllEvents("mv_migrate")
	if err != nil {
		t.Fatalf("ViewAllEvents (pre-migration): %v", err)
	}
	preByHost := collectCountByHost(t, pre, "count")
	if preByHost["web1"] != 2 || preByHost["web2"] != 1 {
		t.Fatalf("pre-migration counts wrong: %v", preByHost)
	}

	// Phase 2: Simulate migration — deactivate, update definition to LynxFlow.
	d.DeactivateView("mv_migrate")
	migratedDef, err := reg.Get("mv_migrate")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	migratedDef.LanguageVersion = "lynxflow"
	migratedDef.Query = `from main | stats count() by host`
	migratedDef.MigratedFrom = `FROM main | stats count by host`

	// IMPORTANT: AggSpec stays the same — this is the storage compatibility contract.
	// The SPL2 AggSpec has Alias="count", and the LynxFlow analysis must produce
	// the same Alias for merge to work.
	if _, lfErr := AnalyzeLynxFlow(migratedDef.Query); lfErr != nil {
		t.Fatalf("AnalyzeLynxFlow for migration: %v", lfErr)
	}
	// For a migrated view, the AggSpec is NOT replaced — we keep the original.
	// This is why MigratedFrom preserves the old query: the AggSpec stays as-is.
	// Only the dispatch path changes to use the LynxFlow physical builder.
	// We do NOT update AggSpec — the migrated view KEEPS the SPL2-derived one.

	if err := reg.Update(migratedDef); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Re-activate with LynxFlow language.
	if err := d.ActivateView(migratedDef); err != nil {
		t.Fatalf("ActivateView (LynxFlow): %v", err)
	}

	// Phase 3: Ingest MORE data through the LynxFlow path.
	batch2 := []*event.Event{
		makeTestEventWithHost("main", "web1"),
		makeTestEventWithHost("main", "web3"),
	}
	d.Dispatch(batch2)

	// Phase 4: ViewAllEvents must merge SPL2-written and LynxFlow-written partial state.
	// Since we kept the SPL2 AggSpec (Alias="count"), both paths serialize
	// to _pa_count_count, so merge works.
	all, err := d.ViewAllEvents("mv_migrate")
	if err != nil {
		t.Fatalf("ViewAllEvents (post-migration): %v", err)
	}

	byHost := collectCountByHost(t, all, "count")

	// web1: 2 (SPL2) + 1 (LynxFlow) = 3
	if byHost["web1"] != 3 {
		t.Errorf("web1 count: got %d, want 3", byHost["web1"])
	}
	// web2: 1 (SPL2 only)
	if byHost["web2"] != 1 {
		t.Errorf("web2 count: got %d, want 1", byHost["web2"])
	}
	// web3: 1 (LynxFlow only)
	if byHost["web3"] != 1 {
		t.Errorf("web3 count: got %d, want 1", byHost["web3"])
	}
}

func TestDispatcher_LynxFlow_RejectsNonAggShape(t *testing.T) {
	// A view with join/dedup/transaction should be rejected.
	tests := []struct {
		name  string
		query string
	}{
		// dedup before aggregate
		{"dedup", `from main | dedup 1 host | stats count() by host`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := AnalyzeLynxFlow(tt.query)
			if err == nil {
				t.Fatalf("expected error for %q", tt.name)
			}
		})
	}
}

func TestDispatcher_LynxFlow_ActivateNeedsMigration(t *testing.T) {
	// A LynxFlow view with unparseable query should be marked needs-migration,
	// not crash.
	d, reg, _ := setupDispatcher(t)
	def := ViewDefinition{
		Name:            "mv_lf_broken",
		Version:         1,
		Type:            ViewTypeAggregation,
		Query:           `this is not valid lynxflow at all !!!`,
		LanguageVersion: "lynxflow",
		Columns: []ColumnDef{
			{Name: "_time", Type: event.FieldTypeTimestamp},
		},
		Status: ViewStatusActive,
	}
	if err := reg.Create(def); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := d.ActivateView(def); err != nil {
		t.Fatalf("ActivateView should succeed (marking as needs-migration): %v", err)
	}
}

// collectCountByHost is a test helper that extracts host -> count mapping.
func collectCountByHost(t *testing.T, events []*event.Event, countField string) map[string]int64 {
	t.Helper()
	m := make(map[string]int64)
	for _, ev := range events {
		host := ev.GetField("host").String()
		count := ev.GetField(countField).AsInt()
		m[host] = count
	}
	return m
}

// TestDispatcher_LynxFlow_TimeBin verifies that a lynxflow view with
// bin(_time, 1h) groups correctly.
func TestDispatcher_LynxFlow_TimeBin(t *testing.T) {
	d, reg, _ := setupDispatcher(t)
	def := createLynxFlowAggView(t, reg, "mv_lf_timebin",
		`from main | stats count() by bin(_time, 1h)`)
	d.ActivateView(def)

	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	events := []*event.Event{
		makeTestEventAtTime("main", base.Add(5*time.Minute)),
		makeTestEventAtTime("main", base.Add(15*time.Minute)),
		makeTestEventAtTime("main", base.Add(65*time.Minute)),
	}
	d.Dispatch(events)

	all, err := d.ViewAllEvents("mv_lf_timebin")
	if err != nil {
		t.Fatalf("ViewAllEvents: %v", err)
	}
	// Should produce 2 time buckets, not 3.
	if len(all) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(all))
	}

	totalCount := int64(0)
	for _, ev := range all {
		totalCount += ev.GetField("count()").AsInt()
	}
	if totalCount != 3 {
		t.Errorf("total count: got %d, want 3", totalCount)
	}
}

// TestPartialAggSpec_LynxFlowSPL2StorageCompat verifies that serialized
// partial state from SPL2 path and LynxFlow path produce the same _pa_ column
// names when using the same AggSpec.
func TestPartialAggSpec_LynxFlowSPL2StorageCompat(t *testing.T) {
	// Shared AggSpec (as it would be for a migrated view).
	spec := &pipeline.PartialAggSpec{
		GroupBy: []string{"host"},
		Funcs: []pipeline.PartialAggFunc{
			{Name: "count", Field: "", Alias: "count"},
			{Name: "sum", Field: "bytes", Alias: "sum(bytes)"},
		},
	}

	groups := []*pipeline.PartialAggGroup{
		{
			Key:    map[string]event.Value{"host": event.StringValue("web1")},
			States: []pipeline.PartialAggState{{Count: 5}, {Sum: 1000, Count: 5}},
		},
	}

	events := PartialGroupsToEvents(groups, spec, "test_view")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	ev := events[0]
	// Verify _pa_ column names.
	if _, ok := ev.Fields["_pa_count_count"]; !ok {
		t.Error("missing _pa_count_count column")
	}
	if _, ok := ev.Fields["_pa_sum(bytes)_sum"]; !ok {
		t.Error("missing _pa_sum(bytes)_sum column")
	}

	// Roundtrip back.
	rt := EventsToPartialGroups(events, spec)
	if len(rt) != 1 {
		t.Fatalf("roundtrip: got %d groups, want 1", len(rt))
	}
	if rt[0].States[0].Count != 5 {
		t.Errorf("count state: got %d, want 5", rt[0].States[0].Count)
	}
	if rt[0].States[1].Sum != 1000 {
		t.Errorf("sum state: got %f, want 1000", rt[0].States[1].Sum)
	}
}
