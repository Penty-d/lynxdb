package views

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
)

// TestViewDefinition_JSONRoundTrip_WithLanguageVersion verifies that the
// LanguageVersion field survives JSON marshal/unmarshal.
func TestViewDefinition_JSONRoundTrip_WithLanguageVersion(t *testing.T) {
	def := ViewDefinition{
		Name:            "mv_test",
		Version:         1,
		Type:            ViewTypeAggregation,
		Query:           "from main | stats count() by level",
		Columns:         []ColumnDef{{Name: "_time", Type: event.FieldTypeTimestamp}},
		Status:          ViewStatusActive,
		LanguageVersion: "lynxflow",
		MigratedFrom:    "level=error | stats count by level",
		CreatedAt:       time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got ViewDefinition
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.LanguageVersion != "lynxflow" {
		t.Errorf("LanguageVersion: got %q, want %q", got.LanguageVersion, "lynxflow")
	}
	if got.MigratedFrom != "level=error | stats count by level" {
		t.Errorf("MigratedFrom: got %q, want %q", got.MigratedFrom, "level=error | stats count by level")
	}
}

// TestViewDefinition_JSONRoundTrip_LegacyNoLanguageVersion verifies that
// old definitions without LanguageVersion load correctly (field absent in
// JSON -> empty string in struct -> EffectiveLanguage returns "spl2").
func TestViewDefinition_JSONRoundTrip_LegacyNoLanguageVersion(t *testing.T) {
	// Simulate a legacy JSON definition without language_version field
	legacyJSON := `{
		"name": "mv_legacy",
		"version": 1,
		"type": 2,
		"query": "level=error | stats count by level",
		"columns": [{"name": "_time", "type": 5}],
		"status": "active",
		"created_at": "2026-06-10T00:00:00Z",
		"updated_at": "2026-06-10T00:00:00Z"
	}`

	var def ViewDefinition
	if err := json.Unmarshal([]byte(legacyJSON), &def); err != nil {
		t.Fatalf("unmarshal legacy JSON: %v", err)
	}

	if def.LanguageVersion != "" {
		t.Errorf("LanguageVersion should be empty for legacy defs, got %q", def.LanguageVersion)
	}
	if def.EffectiveLanguage() != "spl2" {
		t.Errorf("EffectiveLanguage: got %q, want %q", def.EffectiveLanguage(), "spl2")
	}
	if def.MigratedFrom != "" {
		t.Errorf("MigratedFrom should be empty for legacy defs, got %q", def.MigratedFrom)
	}
}

// TestViewDefinition_EffectiveLanguage verifies the EffectiveLanguage helper.
func TestViewDefinition_EffectiveLanguage(t *testing.T) {
	tests := []struct {
		name     string
		langVer  string
		expected string
	}{
		{"empty_is_spl2", "", "spl2"},
		{"explicit_spl2", "spl2", "spl2"},
		{"explicit_lynxflow", "lynxflow", "lynxflow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := ViewDefinition{LanguageVersion: tt.langVer}
			if got := def.EffectiveLanguage(); got != tt.expected {
				t.Errorf("EffectiveLanguage(): got %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestViewDefinition_NeedsMigrationStatus verifies the ViewStatusNeedsMigration
// constant and its string value.
func TestViewDefinition_NeedsMigrationStatus(t *testing.T) {
	def := ViewDefinition{
		Name:   "mv_broken",
		Status: ViewStatusNeedsMigration,
	}
	if def.Status != "needs-migration" {
		t.Errorf("Status: got %q, want %q", def.Status, "needs-migration")
	}
}

// TestViewRegistry_Persistence_LanguageVersion verifies that LanguageVersion
// and MigratedFrom survive a registry persist+reload cycle.
func TestViewRegistry_Persistence_LanguageVersion(t *testing.T) {
	dir := t.TempDir()

	r1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	def := ViewDefinition{
		Name:            "mv_langtest",
		Version:         1,
		Type:            ViewTypeProjection,
		Query:           "from main | stats count() by level",
		Columns:         []ColumnDef{{Name: "_time", Type: event.FieldTypeTimestamp}},
		Status:          ViewStatusActive,
		LanguageVersion: "lynxflow",
		MigratedFrom:    "| stats count by level",
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	if err := r1.Create(def); err != nil {
		t.Fatalf("Create: %v", err)
	}
	r1.Close()

	// Reopen and verify
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer r2.Close()

	got, err := r2.Get("mv_langtest")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.LanguageVersion != "lynxflow" {
		t.Errorf("LanguageVersion: got %q, want %q", got.LanguageVersion, "lynxflow")
	}
	if got.MigratedFrom != "| stats count by level" {
		t.Errorf("MigratedFrom: got %q, want %q", got.MigratedFrom, "| stats count by level")
	}
}
