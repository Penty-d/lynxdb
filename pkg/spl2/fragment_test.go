package spl2

import (
	"fmt"
	"strings"
	"testing"
)

type testFragmentResolver map[string][]Command

func (r testFragmentResolver) Resolve(name string) ([]Command, error) {
	commands, ok := r[name]
	if !ok {
		return nil, fmt.Errorf("fragment %q not found", name)
	}
	return commands, nil
}

func TestExpandFragmentsReplacesUseCommand(t *testing.T) {
	prog, err := ParseProgram(`FROM app | use parse_app | stats count() AS n`)
	if err != nil {
		t.Fatalf("ParseProgram: %v", err)
	}
	fragment, err := parseFragmentBody(`| where level="error" | eval severity = level`)
	if err != nil {
		t.Fatalf("parseFragmentBody: %v", err)
	}

	err = ExpandFragments(prog, testFragmentResolver{"parse_app": fragment})
	if err != nil {
		t.Fatalf("ExpandFragments: %v", err)
	}

	if len(prog.Main.Commands) != 3 {
		t.Fatalf("got %d commands, want 3", len(prog.Main.Commands))
	}
	if _, ok := prog.Main.Commands[0].(*WhereCommand); !ok {
		t.Fatalf("cmd[0]: expected WhereCommand, got %T", prog.Main.Commands[0])
	}
	if _, ok := prog.Main.Commands[1].(*EvalCommand); !ok {
		t.Fatalf("cmd[1]: expected EvalCommand, got %T", prog.Main.Commands[1])
	}
	if _, ok := prog.Main.Commands[2].(*StatsCommand); !ok {
		t.Fatalf("cmd[2]: expected StatsCommand, got %T", prog.Main.Commands[2])
	}
}

func TestExpandFragmentsReportsMissingFragment(t *testing.T) {
	prog, err := ParseProgram(`FROM app | use missing`)
	if err != nil {
		t.Fatalf("ParseProgram: %v", err)
	}

	err = ExpandFragments(prog, testFragmentResolver{})
	if err == nil {
		t.Fatal("expected missing fragment error")
	}
	if !strings.Contains(err.Error(), `use "missing"`) || !strings.Contains(err.Error(), `not found`) {
		t.Fatalf("error %q does not mention missing fragment", err)
	}
}

func TestExpandFragmentsRejectsCycles(t *testing.T) {
	prog, err := ParseProgram(`FROM app | use first`)
	if err != nil {
		t.Fatalf("ParseProgram: %v", err)
	}
	first, err := parseFragmentBody(`| use second`)
	if err != nil {
		t.Fatalf("parse first: %v", err)
	}
	second, err := parseFragmentBody(`| use first`)
	if err != nil {
		t.Fatalf("parse second: %v", err)
	}

	err = ExpandFragments(prog, testFragmentResolver{
		"first":  first,
		"second": second,
	})
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), `circular fragment reference "first"`) {
		t.Fatalf("error %q does not mention cycle", err)
	}
}
