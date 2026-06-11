package shell

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// plainTheme returns a ShellTheme where every style is a no-op, so
// Render returns the input unchanged. This lets us test the token
// reassembly logic without ANSI codes in the output.
func plainTheme() *ShellTheme {
	s := lipgloss.NewStyle()
	return &ShellTheme{
		Command:  s,
		Keyword:  s,
		Function: s,
		String:   s,
		Number:   s,
		Operator: s,
		Pipe:     s,
		Field:    s,
		Error:    s,
		Comment:  s,
	}
}

func TestHighlightSPL2_PreservesInput(t *testing.T) {
	theme := plainTheme()

	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "quoted string field value",
			input: `log_type="postgres" | parse postgres(message) as pg | tail 2`,
		},
		{
			name:  "multiple quoted strings",
			input: `source="nginx" level="error" | stats count by host`,
		},
		{
			name:  "escaped quote in string",
			input: `message="hello \"world\"" | head 10`,
		},
		{
			name:  "empty string",
			input: `field="" | head 5`,
		},
		{
			name:  "no strings",
			input: `level=error | stats count by host | sort -count | head 10`,
		},
		{
			name:  "string at end",
			input: `source="nginx"`,
		},
		{
			name:  "adjacent strings",
			input: `a="one" b="two"`,
		},
		{
			name:  "raw string",
			input: `| where message =~ r"timeout|refused"`,
		},
		{
			name:  "backtick identifiers",
			input: "| keep `my-field`, `another.field`",
		},
		{
			name:  "duration literals",
			input: `| where dur > 5m and age < 1h30m`,
		},
		{
			name:  "line comment",
			input: "from main // this is a comment",
		},
		{
			name:  "complex query",
			input: `from main | where level == "error" and dur > 5m | stats count() by service`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HighlightSPL2(tt.input, theme)
			if got != tt.input {
				t.Errorf("HighlightSPL2 changed the input\n  input: %q\n  got:   %q", tt.input, got)
			}
		})
	}
}

func TestHighlightSPL2_NoDoubledCharacters(t *testing.T) {
	theme := plainTheme()

	input := `log_type="postgres" | tail 2`
	got := HighlightSPL2(input, theme)

	// The old bug produced "postgress" (doubled 's') and lost the opening quote.
	if strings.Contains(got, "postgress") {
		t.Errorf("output contains doubled 's': %q", got)
	}

	if !strings.Contains(got, `"postgres"`) {
		t.Errorf("output missing quoted string \"postgres\": %q", got)
	}
}

func TestHighlightSPL2_EmptyInput(t *testing.T) {
	theme := plainTheme()
	got := HighlightSPL2("", theme)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestHighlightSPL2_NilTheme(t *testing.T) {
	input := "from main | head 10"
	got := HighlightSPL2(input, nil)
	if got != input {
		t.Errorf("nil theme should return input unchanged, got %q", got)
	}
}

// TestClassifyInput_RepresentativeQuery verifies token class assignment for
// a representative query covering all major token classes.
func TestClassifyInput_RepresentativeQuery(t *testing.T) {
	input := `from main | where level == "error" and dur > 5m | stats count() by service`

	spans := classifyInput(input)

	// Build a map from text -> class for easy assertions.
	type spanInfo struct {
		text  string
		class tokenClass
	}
	var infos []spanInfo
	for _, sp := range spans {
		text := input[sp.start:sp.end]
		if strings.TrimSpace(text) == "" {
			continue // skip whitespace
		}
		infos = append(infos, spanInfo{text: text, class: sp.class})
	}

	// Expected token-class pairs (in order).
	expected := []spanInfo{
		{"from", classCommand},
		{"main", classField},
		{"|", classPipe},
		{"where", classCommand},
		{"level", classField},
		{"==", classOperator},
		{`"error"`, classString},
		{"and", classKeyword},
		{"dur", classField},
		{">", classOperator},
		{"5m", classNumber},
		{"|", classPipe},
		{"stats", classCommand},
		{"count", classFunction},
		{"(", classPlain},
		{")", classPlain},
		{"by", classKeyword},
		{"service", classField},
	}

	if len(infos) != len(expected) {
		t.Fatalf("span count mismatch: got %d, want %d\n  got:  %v", len(infos), len(expected), infos)
	}

	for i, want := range expected {
		got := infos[i]
		if got.text != want.text || got.class != want.class {
			t.Errorf("span[%d]: got (%q, %d), want (%q, %d)", i, got.text, got.class, want.text, want.class)
		}
	}
}

// TestClassifyInput_UnterminatedString checks that an unterminated string
// (mid-typing) produces an error token without breaking subsequent spans.
func TestClassifyInput_UnterminatedString(t *testing.T) {
	input := `from main | where level == "err`

	spans := classifyInput(input)

	// The last non-whitespace span should be an error (unterminated string).
	var lastNonWS styledSpan
	for i := len(spans) - 1; i >= 0; i-- {
		text := input[spans[i].start:spans[i].end]
		if strings.TrimSpace(text) != "" {
			lastNonWS = spans[i]
			break
		}
	}

	if lastNonWS.class != classError {
		t.Errorf("unterminated string should be classError, got class=%d, text=%q",
			lastNonWS.class, input[lastNonWS.start:lastNonWS.end])
	}

	// Verify the full input is covered (no gaps).
	assertFullCoverage(t, input, spans)
}

// TestClassifyInput_RawString checks raw string classification.
func TestClassifyInput_RawString(t *testing.T) {
	input := `| where msg =~ r"timeout|refused"`

	spans := classifyInput(input)

	found := false
	for _, sp := range spans {
		text := input[sp.start:sp.end]
		if text == `r"timeout|refused"` {
			if sp.class != classString {
				t.Errorf("raw string should be classString, got %d", sp.class)
			}
			found = true
		}
	}
	if !found {
		t.Error("raw string token not found in spans")
	}

	assertFullCoverage(t, input, spans)
}

// TestClassifyInput_BacktickIdent checks backtick-quoted identifier classification.
func TestClassifyInput_BacktickIdent(t *testing.T) {
	input := "| keep `my-field`"

	spans := classifyInput(input)

	found := false
	for _, sp := range spans {
		text := input[sp.start:sp.end]
		if text == "`my-field`" {
			if sp.class != classField {
				t.Errorf("backtick ident should be classField, got %d", sp.class)
			}
			found = true
		}
	}
	if !found {
		t.Error("backtick ident token not found in spans")
	}

	assertFullCoverage(t, input, spans)
}

// TestClassifyInput_LexFailure checks that input which fails to lex entirely
// still produces spans covering the full input.
func TestClassifyInput_LexFailure(t *testing.T) {
	// Single quote is an error in the lexer.
	input := "from main | where x = 'hello'"

	spans := classifyInput(input)
	assertFullCoverage(t, input, spans)

	// Verify we got at least some valid tokens before the error.
	var hasCommand, hasError bool
	for _, sp := range spans {
		if sp.class == classCommand {
			hasCommand = true
		}
		if sp.class == classError {
			hasError = true
		}
	}
	if !hasCommand {
		t.Error("expected at least one command span")
	}
	if !hasError {
		t.Error("expected at least one error span from single-quote")
	}
}

// TestClassifyInput_TrueFalseNull checks boolean and null literals.
func TestClassifyInput_TrueFalseNull(t *testing.T) {
	input := "| where active == true and deleted == false and tag != null"

	spans := classifyInput(input)

	kwTexts := map[string]bool{"true": false, "false": false, "null": false}
	for _, sp := range spans {
		text := input[sp.start:sp.end]
		if _, ok := kwTexts[text]; ok {
			if sp.class != classKeyword {
				t.Errorf("%q should be classKeyword, got %d", text, sp.class)
			}
			kwTexts[text] = true
		}
	}
	for text, found := range kwTexts {
		if !found {
			t.Errorf("%q not found in spans", text)
		}
	}
}

// TestClassifyInput_FunctionAfterParen checks that ident+( is classified as function.
func TestClassifyInput_FunctionAfterParen(t *testing.T) {
	input := "| stats count() by service | extend x = lower(name)"

	spans := classifyInput(input)

	funcTexts := map[string]bool{"count": false, "lower": false}
	for _, sp := range spans {
		text := input[sp.start:sp.end]
		if _, ok := funcTexts[text]; ok {
			if sp.class != classFunction {
				t.Errorf("%q should be classFunction, got %d", text, sp.class)
			}
			funcTexts[text] = true
		}
	}
	for text, found := range funcTexts {
		if !found {
			t.Errorf("function %q not found in spans", text)
		}
	}
}

// TestClassifyInput_Comment checks comment classification in gaps.
func TestClassifyInput_Comment(t *testing.T) {
	input := "from main // this is a comment"

	spans := classifyInput(input)
	assertFullCoverage(t, input, spans)

	found := false
	for _, sp := range spans {
		text := input[sp.start:sp.end]
		if strings.HasPrefix(text, "//") {
			if sp.class != classComment {
				t.Errorf("comment should be classComment, got %d", sp.class)
			}
			found = true
		}
	}
	if !found {
		t.Error("comment span not found")
	}
}

// TestClassifyInput_Duration checks duration literal classification.
func TestClassifyInput_Duration(t *testing.T) {
	input := "| where dur > 5m"

	spans := classifyInput(input)

	found := false
	for _, sp := range spans {
		text := input[sp.start:sp.end]
		if text == "5m" {
			if sp.class != classNumber {
				t.Errorf("duration 5m should be classNumber, got %d", sp.class)
			}
			found = true
		}
	}
	if !found {
		t.Error("duration 5m not found in spans")
	}
}

// TestClassifyInput_StageNamePosition checks that a keyword in non-stage
// position still gets command styling (stage keywords are always commands).
func TestClassifyInput_StageNamePosition(t *testing.T) {
	input := "from main | where x > 1 | stats count() by service"

	spans := classifyInput(input)

	// Check that all stage keywords get classCommand.
	stageTexts := map[string]bool{"from": false, "where": false, "stats": false}
	for _, sp := range spans {
		text := input[sp.start:sp.end]
		if _, ok := stageTexts[text]; ok {
			if sp.class != classCommand {
				t.Errorf("stage keyword %q should be classCommand, got %d", text, sp.class)
			}
			stageTexts[text] = true
		}
	}
	for text, found := range stageTexts {
		if !found {
			t.Errorf("stage keyword %q not found in spans", text)
		}
	}
}

// TestHighlightSPL2_PrefixNoPanic iterates over every prefix of a long query
// and verifies that highlighting never panics and always covers the full input.
func TestHighlightSPL2_PrefixNoPanic(t *testing.T) {
	query := `from main[-1h] | where level == "error" and dur > 5m | stats count(), avg(dur) by service | sort -count | head 10 | extend ratio = count / total`

	theme := plainTheme()

	for i := 0; i <= len(query); i++ {
		prefix := query[:i]
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on prefix len=%d %q: %v", i, prefix, r)
				}
			}()

			result := HighlightSPL2(prefix, theme)

			// With a plain theme, the result must equal the input.
			if result != prefix {
				t.Errorf("prefix len=%d: result differs\n  input: %q\n  got:   %q", i, prefix, result)
			}

			// Also verify full coverage via classifyInput.
			if prefix != "" {
				spans := classifyInput(prefix)
				assertFullCoverage(t, prefix, spans)
			}
		}()
	}
}

// TestHighlightSPL2_MultiByteUTF8 checks that multi-byte UTF-8 characters
// in field names or strings do not break span alignment.
func TestHighlightSPL2_MultiByteUTF8(t *testing.T) {
	theme := plainTheme()

	input := `from main | where msg == "hello 世界"`
	got := HighlightSPL2(input, theme)
	if got != input {
		t.Errorf("UTF-8 input changed\n  input: %q\n  got:   %q", input, got)
	}

	spans := classifyInput(input)
	assertFullCoverage(t, input, spans)
}

// TestClassifyInput_OperatorsStyled checks various operators.
func TestClassifyInput_OperatorsStyled(t *testing.T) {
	input := "| where a == b and c != d and e >= 1 and f <= 2 and g > 3 and h < 4"

	spans := classifyInput(input)

	opTexts := map[string]bool{"==": false, "!=": false, ">=": false, "<=": false, ">": false, "<": false}
	for _, sp := range spans {
		text := input[sp.start:sp.end]
		if _, ok := opTexts[text]; ok {
			if sp.class != classOperator {
				t.Errorf("operator %q should be classOperator, got %d", text, sp.class)
			}
			opTexts[text] = true
		}
	}
	for text, found := range opTexts {
		if !found {
			t.Errorf("operator %q not found in spans", text)
		}
	}
}

// TestHighlightSPL2_WhitespaceOnly checks that whitespace-only input works.
func TestHighlightSPL2_WhitespaceOnly(t *testing.T) {
	theme := plainTheme()
	input := "   \t  "
	got := HighlightSPL2(input, theme)
	if got != input {
		t.Errorf("whitespace-only input changed\n  input: %q\n  got:   %q", input, got)
	}
}

// TestClassifyInput_UnterminatedBacktick checks unterminated backtick ident.
func TestClassifyInput_UnterminatedBacktick(t *testing.T) {
	input := "| keep `unterminated"

	spans := classifyInput(input)
	assertFullCoverage(t, input, spans)

	var lastNonWS styledSpan
	for i := len(spans) - 1; i >= 0; i-- {
		text := input[spans[i].start:spans[i].end]
		if strings.TrimSpace(text) != "" {
			lastNonWS = spans[i]
			break
		}
	}

	if lastNonWS.class != classError {
		t.Errorf("unterminated backtick should be classError, got class=%d", lastNonWS.class)
	}
}

// TestClassifyInput_UnterminatedRawString checks unterminated raw string.
func TestClassifyInput_UnterminatedRawString(t *testing.T) {
	input := `| where msg =~ r"unterminated`

	spans := classifyInput(input)
	assertFullCoverage(t, input, spans)

	var lastNonWS styledSpan
	for i := len(spans) - 1; i >= 0; i-- {
		text := input[spans[i].start:spans[i].end]
		if strings.TrimSpace(text) != "" {
			lastNonWS = spans[i]
			break
		}
	}

	if lastNonWS.class != classError {
		t.Errorf("unterminated raw string should be classError, got class=%d", lastNonWS.class)
	}
}

// assertFullCoverage verifies that spans cover every byte of input with no gaps
// and no overlaps.
func assertFullCoverage(t *testing.T, input string, spans []styledSpan) {
	t.Helper()

	if len(input) == 0 {
		return
	}

	cursor := 0
	for i, sp := range spans {
		if sp.start != cursor {
			t.Errorf("gap at byte %d (span[%d] starts at %d)", cursor, i, sp.start)
			return
		}
		if sp.end <= sp.start {
			t.Errorf("empty or negative span[%d]: [%d, %d)", i, sp.start, sp.end)
			return
		}
		cursor = sp.end
	}

	if cursor != len(input) {
		t.Errorf("spans end at byte %d but input is %d bytes", cursor, len(input))
	}
}
