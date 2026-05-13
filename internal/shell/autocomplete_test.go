package shell

import (
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/spl2"
)

func TestCompleterCommandCatalogIncludesParserCommands(t *testing.T) {
	completer := NewCompleter()

	have := make(map[string]bool, len(completer.commands))
	for _, command := range completer.commands {
		have[strings.ToLower(command)] = true
	}
	for _, command := range spl2.KnownCommands() {
		if !have[command] {
			t.Fatalf("missing command completion %q", command)
		}
	}
}

func TestCompleterFunctionCatalogIncludesParserFunctions(t *testing.T) {
	completer := NewCompleter()

	haveEval := make(map[string]bool, len(completer.evalFuncs))
	for _, fn := range completer.evalFuncs {
		haveEval[strings.ToLower(fn)] = true
	}
	for _, fn := range appendCatalogs(spl2.KnownEvalFunctions(), spl2.KnownJSONFunctions()) {
		if !haveEval[fn] {
			t.Fatalf("missing eval function completion %q", fn)
		}
	}

	haveAgg := make(map[string]bool, len(completer.aggFuncs))
	for _, fn := range completer.aggFuncs {
		haveAgg[strings.ToLower(fn)] = true
	}
	for _, fn := range spl2.KnownAggregateFunctions() {
		if !haveAgg[fn] {
			t.Fatalf("missing aggregate function completion %q", fn)
		}
	}
}

func TestCompleterSuggestsCompareCommand(t *testing.T) {
	completer := NewCompleter()
	got := completer.Suggest("com")

	for _, suggestion := range got {
		if suggestion == "COMPARE" {
			return
		}
	}

	t.Fatalf("COMPARE not suggested for prefix, got %v", got)
}

func TestCompleterSuggestsEvalFunctionsInEvalContext(t *testing.T) {
	completer := NewCompleter()
	got := completer.Suggest("| eval sha")

	for _, suggestion := range got {
		if suggestion == "| eval sha256" {
			return
		}
	}

	t.Fatalf("sha256 not suggested in eval context, got %v", got)
}

func TestCompleterSuggestsEvalFunctionsInWhereContext(t *testing.T) {
	completer := NewCompleter()
	got := completer.SuggestAll("| where strp")

	for _, item := range got {
		if item.Text == "strptime" && item.Kind == KindFunction {
			return
		}
	}

	t.Fatalf("strptime function item not suggested in where context, got %v", got)
}

func TestCompleterSuggestsJSONFunctionsInEvalContext(t *testing.T) {
	completer := NewCompleter()
	got := completer.Suggest("| eval json_ex")

	for _, suggestion := range got {
		if suggestion == "| eval json_extract" {
			return
		}
	}

	t.Fatalf("json_extract not suggested in eval context, got %v", got)
}
