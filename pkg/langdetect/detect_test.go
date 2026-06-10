package langdetect

import (
	"testing"
)

func TestDetect_ExplicitLynxFlow(t *testing.T) {
	r := Detect("from main | stats count()", "lynxflow")
	if r.Language != LangLynxFlow {
		t.Fatalf("language: got %s, want lynxflow", r.Language)
	}
	if !r.Explicit {
		t.Fatal("expected explicit=true")
	}
}

func TestDetect_ExplicitSPL2(t *testing.T) {
	r := Detect("index=main | stats count", "spl2")
	if r.Language != LangSPL2 {
		t.Fatalf("language: got %s, want spl2", r.Language)
	}
	if !r.Explicit {
		t.Fatal("expected explicit=true")
	}
}

func TestDetect_LynxFlowOnlyQuery(t *testing.T) {
	// CTEs are lynxflow-only syntax.
	r := Detect("let $x = from main | stats count(); from $x", "")
	if r.Language != LangLynxFlow {
		t.Fatalf("language: got %s, want lynxflow", r.Language)
	}
}

func TestDetect_SPL2OnlyQuery(t *testing.T) {
	r := Detect("index=main | stats count", "")
	if r.Language != LangSPL2 {
		t.Fatalf("language: got %s, want spl2", r.Language)
	}
}

func TestDetectStrict_AmbiguousDefaultsSPL2(t *testing.T) {
	// "from main | stats count()" is valid in both languages.
	// DetectStrict should default to spl2 for backward compatibility.
	r := DetectStrict("from main | stats count()", "")
	if r.Language != LangSPL2 {
		t.Fatalf("language: got %s, want spl2 (strict mode)", r.Language)
	}
}

func TestDetectStrict_LynxFlowOnlyRoutes(t *testing.T) {
	r := DetectStrict("let $x = from main | stats count(); from $x", "")
	if r.Language != LangLynxFlow {
		t.Fatalf("language: got %s, want lynxflow", r.Language)
	}
}

func TestDetectStrict_ExplicitLynxFlow(t *testing.T) {
	r := DetectStrict("from main | stats count()", "lynxflow")
	if r.Language != LangLynxFlow {
		t.Fatalf("language: got %s, want lynxflow", r.Language)
	}
	if !r.Explicit {
		t.Fatal("expected explicit=true")
	}
}

func TestValidateExplicitLanguage(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"lynxflow", ""},
		{"spl2", ""},
		{"LynxFlow", ""},
		{"SPL2", ""},
		{"invalid", `invalid language: must be "lynxflow" or "spl2"`},
	}
	for _, tt := range tests {
		got := ValidateExplicitLanguage(tt.input)
		if got != tt.want {
			t.Errorf("ValidateExplicitLanguage(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
