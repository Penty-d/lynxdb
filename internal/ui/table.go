package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"
)

// cellPaddingRight is the right-padding (in chars) applied to each table cell.
const cellPaddingRight = 2

// Table is a styled table renderer built on lipgloss/table.
// It automatically adapts to terminal width: when all columns fit, it renders
// a normal table with cell wrapping. When columns would be too narrow
// (< MinColumnWidth each), it falls back to a vertical record layout that
// preserves every value without truncation.
//
// For interactive scrolling tables, use bubbles/table instead.
type Table struct {
	theme     *Theme
	columns   []string
	rows      [][]string
	kinds     []ColumnKind
	termWidth int
	compact   bool
}

// ColumnKind controls per-column styling and alignment.
type ColumnKind int

const (
	ColumnAuto ColumnKind = iota
	ColumnText
	ColumnNumber
	ColumnDuration
	ColumnBytes
)

// NewTable creates a new table bound to the given theme.
// The terminal width is auto-detected; use SetTerminalWidth to override.
func NewTable(theme *Theme) *Table {
	return &Table{
		theme:     theme,
		termWidth: TerminalWidth(),
	}
}

// SetColumns sets the column headers.
func (t *Table) SetColumns(cols ...string) *Table {
	t.columns = cols

	return t
}

// SetColumnKinds sets optional per-column styles. Missing entries default to text.
func (t *Table) SetColumnKinds(kinds ...ColumnKind) *Table {
	t.kinds = kinds

	return t
}

// SetCompact switches to denser padding and lower narrow-layout thresholds.
func (t *Table) SetCompact(compact bool) *Table {
	t.compact = compact

	return t
}

// SetTerminalWidth overrides the auto-detected terminal width.
// Useful for tests or when rendering to a non-stdout destination.
func (t *Table) SetTerminalWidth(w int) *Table {
	t.termWidth = w

	return t
}

// AddRow appends a row of values.
func (t *Table) AddRow(values ...string) *Table {
	t.rows = append(t.rows, values)

	return t
}

// String renders the table, choosing between table layout and record layout
// based on the terminal width and number of columns.
func (t *Table) String() string {
	if len(t.columns) == 0 {
		return ""
	}

	numCols := len(t.columns)
	// Each column needs at least MinColumnWidth + cellPaddingRight.
	availablePerCol := t.termWidth / numCols
	minColumnWidth := MinColumnWidth
	padding := cellPaddingRight
	if t.compact {
		minColumnWidth = 6
		padding = 1
	}

	if availablePerCol < minColumnWidth+padding {
		return t.renderRecords()
	}

	return t.renderTable()
}

// renderTable renders a standard horizontal table using lipgloss/table,
// constrained to the terminal width with cell wrapping (no truncation).
func (t *Table) renderTable() string {
	// Normalize rows: ensure each row has exactly len(t.columns) cells.
	rows := make([][]string, len(t.rows))
	for i, row := range t.rows {
		r := make([]string, len(t.columns))
		for j := range t.columns {
			if j < len(row) {
				r[j] = row[j]
			}
		}

		rows[i] = r
	}

	// Border with only a header separator line using "─".
	border := lipgloss.Border{
		Top: "\u2500",
	}

	headerStyle := t.theme.TableHeader
	ruleStyle := t.theme.Rule
	padding := cellPaddingRight
	if t.compact {
		padding = 1
	}

	tbl := table.New().
		Border(border).
		BorderStyle(ruleStyle).
		BorderTop(false).
		BorderBottom(false).
		BorderLeft(false).
		BorderRight(false).
		BorderColumn(false).
		BorderRow(false).
		BorderHeader(true).
		Width(t.termWidth).
		Wrap(true).
		StyleFunc(func(row, col int) lipgloss.Style {
			style := lipgloss.NewStyle().PaddingRight(padding)
			if row == table.HeaderRow {
				style = headerStyle.PaddingRight(padding)
			}
			if t.columnKind(col) == ColumnNumber || t.columnKind(col) == ColumnBytes || t.columnKind(col) == ColumnDuration {
				style = style.Align(lipgloss.Right)
			}
			if row != table.HeaderRow {
				switch t.columnKind(col) {
				case ColumnNumber:
					style = style.Foreground(ColorJSONNum())
				case ColumnDuration:
					style = style.Foreground(ColorInfo())
				case ColumnBytes:
					style = style.Foreground(ColorAccent())
				}
			}

			return style
		}).
		Headers(t.columns...).
		Rows(rows...)

	return tbl.String()
}

func (t *Table) columnKind(col int) ColumnKind {
	if col >= 0 && col < len(t.kinds) && t.kinds[col] != ColumnAuto {
		return t.kinds[col]
	}

	return ColumnText
}

// renderRecords renders rows as vertical key-value records. This layout is used
// when the terminal is too narrow for a readable table. Every value is
// preserved in full — nothing is ever truncated.
//
// Example output:
//
//	host:    web-01.prod.example.com
//	status:  200
//	message: Connection established
func (t *Table) renderRecords() string {
	var b strings.Builder

	labelStyle := t.theme.Label
	ruleStyle := t.theme.Rule

	// Find the longest column name for alignment.
	maxLabel := 0
	for _, col := range t.columns {
		if len(col) > maxLabel {
			maxLabel = len(col)
		}
	}

	for i, row := range t.rows {
		header := fmt.Sprintf(" record %d ", i+1)
		remaining := t.termWidth - len(header) - 1
		if remaining < 0 {
			remaining = 0
		}

		line := header + strings.Repeat("\u2500", remaining)
		b.WriteString(ruleStyle.Render(line))
		b.WriteByte('\n')

		// Key-value pairs.
		for j, col := range t.columns {
			val := ""
			if j < len(row) {
				val = row[j]
			}

			label := labelStyle.Render(fmt.Sprintf("  %-*s", maxLabel, col))
			prefix := label + "  "
			prefixWidth := ansi.StringWidth(prefix)
			valueWidth := t.termWidth - prefixWidth
			if valueWidth < 1 {
				valueWidth = 1
			}
			wrappedValue := ansi.Wrap(val, valueWidth, " ")
			wrappedLines := strings.Split(wrappedValue, "\n")
			if len(wrappedLines) == 0 {
				wrappedLines = []string{""}
			}

			b.WriteString(label)
			b.WriteString("  ")
			b.WriteString(wrappedLines[0])
			b.WriteByte('\n')
			continuationPrefix := strings.Repeat(" ", prefixWidth)
			for _, line := range wrappedLines[1:] {
				b.WriteString(continuationPrefix)
				b.WriteString(line)
				b.WriteByte('\n')
			}
		}

		// Blank line between records (but not after the last one).
		if i < len(t.rows)-1 {
			b.WriteByte('\n')
		}
	}

	return b.String()
}
