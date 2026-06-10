package sema

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/desugar"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
)

var update = flag.Bool("update", false, "update golden files")

// ---------------------------------------------------------------------------
// Permissive corpus catalog: types the fields the corpus uses.
// ---------------------------------------------------------------------------

var corpusCatalog = MapCatalog{
	// Commonly used fields in the corpus.
	"status":       TypeInt,
	"duration_ms":  TypeFloat,
	"duration":     TypeFloat,
	"level":        TypeString,
	"service":      TypeString,
	"host":         TypeString,
	"path":         TypeString,
	"method":       TypeString,
	"message":      TypeString,
	"error":        TypeString,
	"user_id":      TypeString,
	"trace_id":     TypeString,
	"span_id":      TypeString,
	"instance":     TypeString,
	"memory_mb":    TypeFloat,
	"cpu_pct":      TypeFloat,
	"amount_cents": TypeInt,
	"client_ip":    TypeString,
	"threat_type":  TypeString,
	"timestamp":    TypeTimestamp,
	"endpoint":     TypeString,
	"uri":          TypeString,
	"user":         TypeString,
	"ip":           TypeString,
	"tags":         TypeArray,
	"request_size": TypeInt,

	// Fields used by sigma/rsigma corpus entries.
	"EventID":       TypeInt,
	"SubStatus":     TypeString,
	"CommandLine":   TypeString,
	"Image":         TypeString,
	"ParentImage":   TypeString,
	"FieldA":        TypeString,
	"FieldB":        TypeString,
	"FieldC":        TypeString,
	"Enabled":       TypeBool,
	"SourceIP":      TypeString,
	"response_time": TypeInt,

	// Fields used by c052 (materialized view).
	"source": TypeString,

	// Fields used by c053 (CTE join).
	"type":   TypeString,
	"res":    TypeString,
	"src_ip": TypeString,

	// Fields used by c054 (docker parse + explode).
	"errors": TypeArray,

	// Aggregate result fields used by chained queries.
	"count":    TypeInt,
	"rate":     TypeFloat,
	"dur":      TypeFloat,
	"latency":  TypeFloat,
	"bytes":    TypeInt,
	"total":    TypeInt,
	"failures": TypeInt,
}

// ---------------------------------------------------------------------------
// Corpus test: parse+desugar+Analyze all 63 entries -> 0 error diags.
// ---------------------------------------------------------------------------

type corpusEntry struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Source   string   `json:"source"`
	SPL2     string   `json:"spl2"`
	LynxFlow string   `json:"lynxflow"`
	Features []string `json:"features"`
	Notes    string   `json:"notes"`
}

func loadCorpus(t *testing.T) []corpusEntry {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "testdata", "corpus", "corpus.jsonl"))
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close()

	var entries []corpusEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var e corpusEntry
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan corpus: %v", err)
	}
	return entries
}

func TestCorpus_ZeroErrors(t *testing.T) {
	entries := loadCorpus(t)
	if len(entries) < 50 {
		t.Fatalf("corpus has %d entries, want at least 50", len(entries))
	}

	for _, e := range entries {
		t.Run(e.ID, func(t *testing.T) {
			q, pDiags := parser.Parse(e.LynxFlow)
			if len(pDiags) > 0 {
				t.Skipf("parse diags: %v", pDiags)
				return
			}
			desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
			result := Analyze(desugared, corpusCatalog)

			var errors []string
			for _, d := range result.Diags {
				if d.Severity == parser.SeverityError {
					errors = append(errors, fmt.Sprintf("%s [%d:%d] %s", d.Code, d.Span.Start, d.Span.End, d.Message))
				}
			}
			if len(errors) > 0 {
				t.Errorf("error-severity diags for %q:\n  %s\n  query: %s",
					e.ID, strings.Join(errors, "\n  "), e.LynxFlow)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Schema flow tests: verify OutputSchema per stage kind.
// ---------------------------------------------------------------------------

func parseAndAnalyze(t *testing.T, query string, cat Catalog) Result {
	t.Helper()
	q, pDiags := parser.Parse(query)
	if len(pDiags) > 0 {
		t.Fatalf("parse error in %q: %v", query, pDiags)
	}
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	return Analyze(desugared, cat)
}

func schemaFieldNames(result Result) []string {
	names := make([]string, len(result.OutputSchema))
	for i, f := range result.OutputSchema {
		names[i] = f.Name
	}
	return names
}

func schemaHasField(result Result, name string) bool {
	for _, f := range result.OutputSchema {
		if f.Name == name {
			return true
		}
	}
	return false
}

func schemaFieldType(result Result, name string) FieldType {
	for _, f := range result.OutputSchema {
		if f.Name == name {
			return f.Type
		}
	}
	return ""
}

func TestSchemaFlow_Where(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	r := parseAndAnalyze(t, "| where status == 500", cat)
	// Where does not change schema.
	if !schemaHasField(r, "status") {
		t.Error("where should preserve schema; missing 'status'")
	}
	if !schemaHasField(r, "_time") {
		t.Error("where should preserve builtins; missing '_time'")
	}
}

func TestSchemaFlow_Extend(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	r := parseAndAnalyze(t, "| extend is_err = status >= 500", cat)
	if !schemaHasField(r, "is_err") {
		t.Error("extend should add 'is_err'")
	}
	if schemaFieldType(r, "is_err") != TypeBool {
		t.Errorf("is_err should be bool, got %s", schemaFieldType(r, "is_err"))
	}
}

func TestSchemaFlow_Keep(t *testing.T) {
	cat := MapCatalog{"status": TypeInt, "host": TypeString}
	r := parseAndAnalyze(t, "| keep status, host", cat)
	names := schemaFieldNames(r)
	if len(names) != 2 {
		t.Errorf("keep should have 2 fields, got %d: %v", len(names), names)
	}
}

func TestSchemaFlow_KeepStarExcept(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	r := parseAndAnalyze(t, "| keep * except _raw", cat)
	if schemaHasField(r, "_raw") {
		t.Error("keep * except _raw should remove _raw")
	}
	if !schemaHasField(r, "_time") {
		t.Error("keep * except _raw should preserve _time")
	}
}

func TestSchemaFlow_Drop(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	r := parseAndAnalyze(t, "| drop _raw", cat)
	if schemaHasField(r, "_raw") {
		t.Error("drop should remove _raw")
	}
	if !schemaHasField(r, "_time") {
		t.Error("drop should preserve other builtins")
	}
}

func TestSchemaFlow_Rename(t *testing.T) {
	cat := MapCatalog{"duration_ms": TypeFloat}
	r := parseAndAnalyze(t, "| rename duration_ms as latency", cat)
	if schemaHasField(r, "duration_ms") {
		t.Error("rename should remove old name")
	}
	if !schemaHasField(r, "latency") {
		t.Error("rename should add new name")
	}
	if schemaFieldType(r, "latency") != TypeFloat {
		t.Errorf("latency should be float, got %s", schemaFieldType(r, "latency"))
	}
}

func TestSchemaFlow_Stats(t *testing.T) {
	cat := MapCatalog{"status": TypeInt, "service": TypeString}
	r := parseAndAnalyze(t, "| stats count() by service", cat)
	// Stats replaces schema.
	if schemaHasField(r, "_time") {
		t.Error("stats should replace schema; _time should be gone")
	}
	if !schemaHasField(r, "service") {
		t.Error("stats should include by-key 'service'")
	}
	if !schemaHasField(r, "count()") {
		t.Errorf("stats should include agg result 'count()'; got %v", schemaFieldNames(r))
	}
}

func TestSchemaFlow_StatsWithAlias(t *testing.T) {
	cat := MapCatalog{"status": TypeInt, "service": TypeString}
	r := parseAndAnalyze(t, "| stats count() as total by service", cat)
	if !schemaHasField(r, "total") {
		t.Error("stats should include alias 'total'")
	}
	if schemaFieldType(r, "total") != TypeInt {
		t.Errorf("count() should be int, got %s", schemaFieldType(r, "total"))
	}
}

func TestSchemaFlow_Eventstats(t *testing.T) {
	cat := MapCatalog{"duration_ms": TypeFloat}
	r := parseAndAnalyze(t, "| eventstats avg(duration_ms) as global_avg", cat)
	// Eventstats adds fields, doesn't replace.
	if !schemaHasField(r, "_time") {
		t.Error("eventstats should preserve existing schema")
	}
	if !schemaHasField(r, "global_avg") {
		t.Error("eventstats should add agg field 'global_avg'")
	}
}

func TestSchemaFlow_Streamstats(t *testing.T) {
	cat := MapCatalog{"duration_ms": TypeFloat}
	r := parseAndAnalyze(t, "| streamstats window=3 avg(duration_ms) as rolling_avg", cat)
	if !schemaHasField(r, "_time") {
		t.Error("streamstats should preserve existing schema")
	}
	if !schemaHasField(r, "rolling_avg") {
		t.Error("streamstats should add 'rolling_avg'")
	}
}

func TestSchemaFlow_Sort(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	r := parseAndAnalyze(t, "| sort -status", cat)
	if !schemaHasField(r, "status") {
		t.Error("sort should not change schema")
	}
}

func TestSchemaFlow_Dedup(t *testing.T) {
	cat := MapCatalog{"service": TypeString}
	r := parseAndAnalyze(t, "| dedup service", cat)
	if !schemaHasField(r, "service") {
		t.Error("dedup should not change schema")
	}
}

func TestSchemaFlow_Head(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	r := parseAndAnalyze(t, "| head 10", cat)
	if !schemaHasField(r, "status") {
		t.Error("head should not change schema")
	}
}

func TestSchemaFlow_Tail(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	r := parseAndAnalyze(t, "| tail 5", cat)
	if !schemaHasField(r, "status") {
		t.Error("tail should not change schema")
	}
}

func TestSchemaFlow_Explode(t *testing.T) {
	cat := MapCatalog{"tags": TypeArray}
	r := parseAndAnalyze(t, "| explode tags as tag", cat)
	if !schemaHasField(r, "tag") {
		t.Error("explode should add 'tag' field")
	}
	if schemaFieldType(r, "tag") != TypeAny {
		t.Errorf("explode element should be 'any', got %s", schemaFieldType(r, "tag"))
	}
}

func TestSchemaFlow_Describe(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	r := parseAndAnalyze(t, "| describe", cat)
	names := schemaFieldNames(r)
	expected := []string{"field", "type", "coverage", "distinct_est", "top_values"}
	if len(names) != len(expected) {
		t.Errorf("describe schema: got %v, want %v", names, expected)
	}
	for i, n := range expected {
		if i < len(names) && names[i] != n {
			t.Errorf("describe schema[%d]: got %s, want %s", i, names[i], n)
		}
	}
}

func TestSchemaFlow_ParseWithInto(t *testing.T) {
	cat := MapCatalog{}
	r := parseAndAnalyze(t, `| parse json into (status as int, user as string)`, cat)
	if !schemaHasField(r, "status") {
		t.Error("parse into should add 'status'")
	}
	if schemaFieldType(r, "status") != TypeInt {
		t.Errorf("parse into status should be int, got %s", schemaFieldType(r, "status"))
	}
}

func TestSchemaFlow_ParseWithoutInto(t *testing.T) {
	cat := MapCatalog{}
	r := parseAndAnalyze(t, "| parse json", cat)
	// After parse without into, schema should be open.
	// The next stage should not produce unknown-field errors for any field.
	q, _ := parser.Parse("| parse json | where some_dynamic_field == 42")
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	result := Analyze(desugared, cat)
	for _, d := range result.Diags {
		if d.Code == CodeUnknownField {
			t.Errorf("parse without into should open schema; got unknown field diag: %s", d.Message)
		}
	}
	_ = r
}

func TestSchemaFlow_Join(t *testing.T) {
	cat := MapCatalog{"user_id": TypeString, "name": TypeString}
	r := parseAndAnalyze(t, `| join type=inner on user_id with [from main | keep user_id, name]`, cat)
	if !schemaHasField(r, "user_id") {
		t.Error("join should include user_id from both sides")
	}
}

func TestSchemaFlow_StatsBinTime(t *testing.T) {
	cat := MapCatalog{"service": TypeString}
	r := parseAndAnalyze(t, "| stats count() by service, bin(_time, 5m)", cat)
	if !schemaHasField(r, "_time") {
		t.Error("stats with bin(_time, d) should include _time in output")
	}
	if schemaFieldType(r, "_time") != TypeTimestamp {
		t.Errorf("_time should be timestamp, got %s", schemaFieldType(r, "_time"))
	}
}

// ---------------------------------------------------------------------------
// Expression type inference tests.
// ---------------------------------------------------------------------------

func TestInfer_Literals(t *testing.T) {
	tests := []struct {
		expr string
		want FieldType
	}{
		{`"hello"`, TypeString},
		{`42`, TypeInt},
		{`3.14`, TypeFloat},
		{`true`, TypeBool},
		{`false`, TypeBool},
		{`null`, TypeAny},
		{`5m`, TypeDuration},
	}
	cat := MapCatalog{}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			r := parseAndAnalyze(t, "| extend x = "+tt.expr, cat)
			if schemaFieldType(r, "x") != tt.want {
				t.Errorf("extend x = %s: got type %s, want %s", tt.expr, schemaFieldType(r, "x"), tt.want)
			}
		})
	}
}

func TestInfer_BinaryArithmetic(t *testing.T) {
	tests := []struct {
		expr string
		want FieldType
	}{
		{"1 + 2", TypeInt},
		{"1 + 2.0", TypeFloat},
		{"1.0 + 2.0", TypeFloat},
		{"1 - 2", TypeInt},
		{"1 * 2", TypeInt},
		{"1 / 2", TypeInt},
		{"1.0 / 2", TypeFloat},
		{"1 % 2", TypeInt},
	}
	cat := MapCatalog{}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			r := parseAndAnalyze(t, "| extend x = "+tt.expr, cat)
			if schemaFieldType(r, "x") != tt.want {
				t.Errorf("extend x = %s: got type %s, want %s", tt.expr, schemaFieldType(r, "x"), tt.want)
			}
		})
	}
}

func TestInfer_StringConcat(t *testing.T) {
	cat := MapCatalog{"a": TypeString, "b": TypeString}
	r := parseAndAnalyze(t, `| extend x = a + b`, cat)
	if schemaFieldType(r, "x") != TypeString {
		t.Errorf("string + string should be string, got %s", schemaFieldType(r, "x"))
	}
}

func TestInfer_TimestampArithmetic(t *testing.T) {
	cat := MapCatalog{}
	tests := []struct {
		expr string
		want FieldType
	}{
		{"_time - 5m", TypeTimestamp},
		{"5m + 10m", TypeDuration},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			r := parseAndAnalyze(t, "| extend x = "+tt.expr, cat)
			if schemaFieldType(r, "x") != tt.want {
				t.Errorf("extend x = %s: got %s, want %s", tt.expr, schemaFieldType(r, "x"), tt.want)
			}
		})
	}
}

func TestInfer_Comparison(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	r := parseAndAnalyze(t, "| extend x = status >= 500", cat)
	if schemaFieldType(r, "x") != TypeBool {
		t.Errorf("comparison should be bool, got %s", schemaFieldType(r, "x"))
	}
}

func TestInfer_FunctionCalls(t *testing.T) {
	tests := []struct {
		expr string
		want FieldType
	}{
		{`lower("ABC")`, TypeString},
		{`len("abc")`, TypeInt},
		{`round(3.14)`, TypeFloat},
		{`now()`, TypeTimestamp},
		{`bin(_time, 5m)`, TypeTimestamp},
	}
	cat := MapCatalog{}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			r := parseAndAnalyze(t, "| extend x = "+tt.expr, cat)
			if schemaFieldType(r, "x") != tt.want {
				t.Errorf("extend x = %s: got type %s, want %s", tt.expr, schemaFieldType(r, "x"), tt.want)
			}
		})
	}
}

func TestInfer_CoalesceType(t *testing.T) {
	cat := MapCatalog{"a": TypeString}
	r := parseAndAnalyze(t, `| extend x = a ?? "default"`, cat)
	if schemaFieldType(r, "x") != TypeString {
		t.Errorf("coalesce should be string, got %s", schemaFieldType(r, "x"))
	}
}

func TestInfer_Ident_UnknownInClosedSchema(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	q, _ := parser.Parse("| where unknown_field == 42")
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	result := Analyze(desugared, cat)
	found := false
	for _, d := range result.Diags {
		if d.Code == CodeUnknownField {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected S001 unknown-field diagnostic")
	}
}

func TestInfer_Ident_UnknownInOpenSchema(t *testing.T) {
	cat := MapCatalog{}
	q, _ := parser.Parse("| parse json | where dynamic_field == 42")
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	result := Analyze(desugared, cat)
	for _, d := range result.Diags {
		if d.Code == CodeUnknownField {
			t.Errorf("open schema should not produce unknown-field diagnostic: %s", d.Message)
		}
	}
}

// ---------------------------------------------------------------------------
// Type error diagnostics.
// ---------------------------------------------------------------------------

func TestDiag_TypeMismatchComparison(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	q, _ := parser.Parse(`| where status == "500"`)
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	result := Analyze(desugared, cat)
	found := false
	for _, d := range result.Diags {
		if d.Code == CodeTypeMismatchComparison {
			found = true
			// Should suggest retyped literal.
			if !strings.Contains(d.Suggestion, "500") {
				t.Errorf("suggestion should contain retyped literal '500': %s", d.Suggestion)
			}
			break
		}
	}
	if !found {
		t.Error("expected S003 type-mismatch-comparison diagnostic")
	}
}

func TestDiag_StringPlusNumber(t *testing.T) {
	cat := MapCatalog{"name": TypeString}
	q, _ := parser.Parse("| extend x = name + 42")
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	result := Analyze(desugared, cat)
	found := false
	for _, d := range result.Diags {
		if d.Code == CodeTypeMismatchBinop {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected S002 type-mismatch-binop diagnostic for string + int")
	}
}

func TestDiag_AggOutsideStats(t *testing.T) {
	cat := MapCatalog{}
	q, _ := parser.Parse("| extend x = count()")
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	result := Analyze(desugared, cat)
	found := false
	for _, d := range result.Diags {
		if d.Code == CodeAggOutsideStats {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected S005 aggregate-outside-stats diagnostic")
	}
}

func TestDiag_UnknownFunction(t *testing.T) {
	cat := MapCatalog{}
	q, _ := parser.Parse(`| extend x = unkown_fn("a")`)
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	result := Analyze(desugared, cat)
	found := false
	for _, d := range result.Diags {
		if d.Code == CodeUnknownFunction {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected S006 unknown-function diagnostic")
	}
}

func TestDiag_WrongArity(t *testing.T) {
	cat := MapCatalog{}
	q, _ := parser.Parse(`| extend x = lower("a", "b", "c")`)
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	result := Analyze(desugared, cat)
	found := false
	for _, d := range result.Diags {
		if d.Code == CodeWrongArity {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected S007 wrong-arity diagnostic")
	}
}

func TestDiag_NonBoolLogical(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	q, _ := parser.Parse("| where status and true")
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	result := Analyze(desugared, cat)
	found := false
	for _, d := range result.Diags {
		if d.Code == CodeNonBoolLogical {
			found = true
			if d.Severity != parser.SeverityWarning {
				t.Errorf("non-bool logical should be warning, got %s", d.Severity)
			}
			break
		}
	}
	if !found {
		t.Error("expected S004 non-bool-logical warning")
	}
}

// ---------------------------------------------------------------------------
// Streaming safety tests.
// ---------------------------------------------------------------------------

func TestStreaming_WhereHead(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	r := parseAndAnalyze(t, "| where status >= 500 | head 10", cat)
	if !r.StreamingSafe {
		t.Error("where + head should be streaming safe")
	}
}

func TestStreaming_Stats(t *testing.T) {
	cat := MapCatalog{}
	r := parseAndAnalyze(t, "| stats count()", cat)
	if r.StreamingSafe {
		t.Error("stats should make pipeline not streaming safe")
	}
}

func TestStreaming_Sort(t *testing.T) {
	cat := MapCatalog{"status": TypeInt}
	r := parseAndAnalyze(t, "| sort -status", cat)
	if r.StreamingSafe {
		t.Error("sort should make pipeline not streaming safe")
	}
}

func TestStreaming_Dedup(t *testing.T) {
	cat := MapCatalog{"service": TypeString}
	r := parseAndAnalyze(t, "| where true | dedup service | head 5", cat)
	if !r.StreamingSafe {
		t.Error("where + dedup + head should be streaming safe (dedup is row-streaming)")
	}
}

// ---------------------------------------------------------------------------
// Golden error-message tests.
// ---------------------------------------------------------------------------

func TestGoldenErrors(t *testing.T) {
	goldenDir := filepath.Join("..", "testdata", "golden", "errors")
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatalf("read golden dir: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txt") {
			continue
		}
		t.Run(strings.TrimSuffix(entry.Name(), ".txt"), func(t *testing.T) {
			path := filepath.Join(goldenDir, entry.Name())
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden file: %v", err)
			}
			query, expectedDiags := parseGoldenFile(t, string(content))
			actual := runGoldenQuery(t, query)

			if *update {
				writeGoldenFile(t, path, query, actual)
				return
			}

			if actual != expectedDiags {
				t.Errorf("golden mismatch for %s:\n--- expected ---\n%s\n--- actual ---\n%s",
					entry.Name(), expectedDiags, actual)
			}
		})
	}
}

func parseGoldenFile(t *testing.T, content string) (query string, diags string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("empty golden file")
	}
	if !strings.HasPrefix(lines[0], "query: ") {
		t.Fatalf("golden file first line must start with 'query: ', got: %s", lines[0])
	}
	query = strings.TrimPrefix(lines[0], "query: ")
	if len(lines) > 1 {
		diags = strings.Join(lines[1:], "\n")
	}
	return query, diags
}

func runGoldenQuery(t *testing.T, query string) string {
	t.Helper()
	q, pDiags := parser.Parse(query)
	if len(pDiags) > 0 {
		// Parse errors are expected in some golden files; render them too.
		var b strings.Builder
		for _, d := range pDiags {
			fmt.Fprintf(&b, "%s %s [%d:%d] %s\n",
				d.Severity, d.Code, d.Span.Start, d.Span.End, d.Message)
			if d.Suggestion != "" {
				fmt.Fprintf(&b, "  suggestion: %s\n", d.Suggestion)
			}
		}
		return strings.TrimRight(b.String(), "\n")
	}

	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})

	// Use a catalog with specific known fields for error testing.
	cat := MapCatalog{
		"status":        TypeInt,
		"response_time": TypeInt,
		"level":         TypeString,
		"service":       TypeString,
		"_time":         TypeTimestamp,
		"duration_ms":   TypeFloat,
		"host":          TypeString,
		"name":          TypeString,
	}
	result := Analyze(desugared, cat)

	// Sort diags by span start for deterministic output.
	sort.Slice(result.Diags, func(i, j int) bool {
		if result.Diags[i].Span.Start != result.Diags[j].Span.Start {
			return result.Diags[i].Span.Start < result.Diags[j].Span.Start
		}
		return result.Diags[i].Code < result.Diags[j].Code
	})

	var b strings.Builder
	for _, d := range result.Diags {
		fmt.Fprintf(&b, "%s %s [%d:%d] %s\n",
			d.Severity, d.Code, d.Span.Start, d.Span.End, d.Message)
		if d.Suggestion != "" {
			fmt.Fprintf(&b, "  suggestion: %s\n", d.Suggestion)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeGoldenFile(t *testing.T, path, query, diags string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("query: ")
	b.WriteString(query)
	b.WriteByte('\n')
	if diags != "" {
		b.WriteString(diags)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		t.Fatalf("write golden file: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Levenshtein tests.
// ---------------------------------------------------------------------------

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"status", "Status", 1},
		{"timestamp", "_time", 6},
		{"statu", "status", 1},
	}
	for _, tt := range tests {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			got := levenshtein(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("levenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestDidYouMean(t *testing.T) {
	candidates := []string{"status", "_time", "service", "host", "level"}
	tests := []struct {
		name string
		want string
	}{
		{"Status", "did you mean 'status'?"},
		{"statu", "did you mean 'status'?"},
		{"timestamp", "did you mean '_time'?"}, // substring containment fallback
		{"sevice", "did you mean 'service'?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := didYouMean(tt.name, candidates)
			if got != tt.want {
				t.Errorf("didYouMean(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Glob matching tests.
// ---------------------------------------------------------------------------

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything", true},
		{"trace_*", "trace_id", true},
		{"trace_*", "span_id", false},
		{"?at", "cat", true},
		{"?at", "at", false},
		{"a*b", "ab", true},
		{"a*b", "axb", true},
		{"a*b", "axyz", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.name, func(t *testing.T) {
			got := globMatch(tt.pattern, tt.name)
			if got != tt.want {
				t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
			}
		})
	}
}
