package parser

import "testing"

func TestSearchSugarGlobWithDash(t *testing.T) {
	for _, q := range []string{
		`from main host=web-*`,
		`from main host=web-* errors`,
		`from logs*,!logs-debug*[-1h] panic`,
	} {
		_, diags := Parse(q)
		if len(diags) != 0 {
			t.Errorf("%s: diags = %v", q, diags)
		}
	}
}
