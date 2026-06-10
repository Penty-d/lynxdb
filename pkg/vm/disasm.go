package vm

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Disassemble returns a human-readable, deterministic text representation of a
// compiled Program. The output includes the instruction stream, constant pool,
// field names, regex patterns, and CIDR nets. This is used by golden tests to
// assert byte-identical compilation output across refactors.
func Disassemble(p *Program) string {
	var b strings.Builder

	// Header: pools.
	b.WriteString("=== constants ===\n")
	for i, c := range p.Constants {
		fmt.Fprintf(&b, "  [%d] %s\n", i, c)
	}

	b.WriteString("=== fields ===\n")
	for i, f := range p.FieldNames {
		fmt.Fprintf(&b, "  [%d] %s\n", i, f)
	}

	b.WriteString("=== regexes ===\n")
	for i, r := range p.RegexPatterns {
		fmt.Fprintf(&b, "  [%d] %s\n", i, r)
	}

	b.WriteString("=== cidrs ===\n")
	for i, n := range p.CIDRNets {
		fmt.Fprintf(&b, "  [%d] %s\n", i, n)
	}

	fmt.Fprintf(&b, "=== bsi_handled_comparisons: %d ===\n", p.BSIHandledComparisons)

	b.WriteString("=== instructions ===\n")
	ins := p.Instructions
	offset := 0
	for offset < len(ins) {
		op := Opcode(ins[offset])
		def, ok := definitions[op]
		if !ok {
			fmt.Fprintf(&b, "  %04d: UNKNOWN(0x%02x)\n", offset, byte(op))
			offset++
			continue
		}

		// Calculate operand width.
		operandSize := 0
		for _, w := range def.OperandWidths {
			operandSize += w
		}

		if offset+1+operandSize > len(ins) {
			fmt.Fprintf(&b, "  %04d: %s [TRUNCATED]\n", offset, def.Name)
			break
		}

		if len(def.OperandWidths) == 0 {
			fmt.Fprintf(&b, "  %04d: %s\n", offset, def.Name)
		} else {
			// Read operands.
			var operands []string
			pos := offset + 1
			for _, w := range def.OperandWidths {
				switch w {
				case 1:
					operands = append(operands, fmt.Sprintf("%d", ins[pos]))
					pos++
				case 2:
					v := binary.BigEndian.Uint16(ins[pos : pos+2])
					operands = append(operands, fmt.Sprintf("%d", v))
					pos += 2
				}
			}
			fmt.Fprintf(&b, "  %04d: %s %s\n", offset, def.Name, strings.Join(operands, " "))
		}

		offset += 1 + operandSize
	}

	return b.String()
}
