package main

import "testing"

func TestShellCommandHasAliases(t *testing.T) {
	cmd := newShellCmd()
	want := map[string]bool{
		"sh":      false,
		"repl":    false,
		"console": false,
	}

	for _, alias := range cmd.Aliases {
		if _, ok := want[alias]; ok {
			want[alias] = true
		}
	}

	for alias, found := range want {
		if !found {
			t.Fatalf("shell command aliases = %v, want %s", cmd.Aliases, alias)
		}
	}
}
