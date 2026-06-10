package logical

import (
	"fmt"
	"strings"
)

// Plan is the top-level logical plan for a query.
type Plan struct {
	// Root is the terminal node of the main pipeline.
	Root Node
	// Lets maps CTE names (without $) to their lowered sub-plans.
	// CTE references in the main pipeline or other CTEs share pointers
	// into this map; physical planning may materialize shared subplans.
	Lets map[string]*Plan
}

// Dump produces a deterministic, human-readable multi-line rendering of the
// plan tree. Each node is printed on one line with two-space indentation per
// nesting level. This is the format used by golden plan tests.
func (p *Plan) Dump() string {
	var b strings.Builder
	if len(p.Lets) > 0 {
		// Sort lets for determinism.
		keys := sortedKeys(p.Lets)
		for _, name := range keys {
			fmt.Fprintf(&b, "Let $%s\n", name)
			dumpNode(&b, p.Lets[name].Root, 1)
		}
	}
	dumpNode(&b, p.Root, 0)
	return b.String()
}

func dumpNode(b *strings.Builder, n Node, depth int) {
	if n == nil {
		indent(b, depth)
		b.WriteString("<nil>\n")
		return
	}
	indent(b, depth)
	b.WriteString(n.String())
	b.WriteByte('\n')

	// For Join, print right sub-plan under a "Right:" label.
	if j, ok := n.(*Join); ok {
		// Print left child (from Children).
		for _, c := range n.Children() {
			dumpNode(b, c, depth+1)
		}
		indent(b, depth+1)
		b.WriteString("Right:\n")
		dumpNode(b, j.Right, depth+2)
		return
	}

	// For Union, print all inputs.
	for _, c := range n.Children() {
		dumpNode(b, c, depth+1)
	}
}

func indent(b *strings.Builder, depth int) {
	for i := 0; i < depth; i++ {
		b.WriteString("  ")
	}
}

func sortedKeys(m map[string]*Plan) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Simple insertion sort (small maps).
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
