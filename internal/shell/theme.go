package shell

import (
	"charm.land/lipgloss/v2"

	"github.com/lynxbase/lynxdb/internal/ui"
)

// ShellTheme holds syntax highlighting styles for LynxFlow queries.
type ShellTheme struct {
	Command  lipgloss.Style // cyan bold — stage operators (from, where, stats)
	Keyword  lipgloss.Style // magenta — clause keywords (by, as, and, or)
	Function lipgloss.Style // yellow — scalar/aggregate functions
	String   lipgloss.Style // green — quoted strings
	Number   lipgloss.Style // yellow — numeric literals and durations
	Operator lipgloss.Style // white bold — comparison operators
	Pipe     lipgloss.Style // bright white bold — |
	Field    lipgloss.Style // default — identifiers/field names
	Error    lipgloss.Style // red — lexer errors (unterminated strings etc.)
	Comment  lipgloss.Style // dim — line/block comments
}

// NewShellTheme creates a ShellTheme derived from the centralized ui colors.
func NewShellTheme() *ShellTheme {
	return &ShellTheme{
		Command:  lipgloss.NewStyle().Foreground(ui.ColorInfo()).Bold(true),
		Keyword:  lipgloss.NewStyle().Foreground(ui.ColorAccent()),
		Function: lipgloss.NewStyle().Foreground(ui.ColorWarning()),
		String:   lipgloss.NewStyle().Foreground(ui.ColorJSONStr()),
		Number:   lipgloss.NewStyle().Foreground(ui.ColorJSONNum()),
		Operator: lipgloss.NewStyle().Foreground(ui.ColorWhite()).Bold(true),
		Pipe:     lipgloss.NewStyle().Foreground(ui.ColorWhite()).Bold(true),
		Field:    lipgloss.NewStyle(),
		Error:    lipgloss.NewStyle().Foreground(ui.ColorError()),
		Comment:  lipgloss.NewStyle().Foreground(ui.ColorDark()),
	}
}

// NewShellEditorTheme makes identifiers visible while typing. The scrollback
// theme keeps fields plain so completed queries stay quieter.
func NewShellEditorTheme() *ShellTheme {
	return &ShellTheme{
		Command:  lipgloss.NewStyle().Foreground(ui.ColorInfo()).Bold(true),
		Keyword:  lipgloss.NewStyle().Foreground(ui.ColorAccent()).Bold(true),
		Function: lipgloss.NewStyle().Foreground(ui.ColorWarning()).Bold(true),
		String:   lipgloss.NewStyle().Foreground(ui.ColorJSONStr()),
		Number:   lipgloss.NewStyle().Foreground(ui.ColorJSONNum()),
		Operator: lipgloss.NewStyle().Foreground(ui.ColorAccent()).Bold(true),
		Pipe:     lipgloss.NewStyle().Foreground(ui.ColorInfo()).Bold(true),
		Field:    lipgloss.NewStyle().Foreground(ui.ColorWhite()),
		Error:    lipgloss.NewStyle().Foreground(ui.ColorError()),
		Comment:  lipgloss.NewStyle().Foreground(ui.ColorDark()).Italic(true),
	}
}
