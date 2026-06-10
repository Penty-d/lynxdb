package parser

import "testing"

func TestQuotedCalleeDiags(t *testing.T) {
	_, diags := Parse("from main | where `A`(1)")
	if len(diags) == 0 {
		t.Fatal("quoted callee parsed silently")
	}
}
