package pipeline

import "testing"

func TestParseCommandForFormat_DoesNotEmitUnsupportedDelimitedParsers(t *testing.T) {
	for _, format := range []DetectedFormat{FormatCSV, FormatTSV} {
		if got := ParseCommandForFormat(format); got != "" {
			t.Fatalf("ParseCommandForFormat(%s) = %q, want empty", format, got)
		}
	}
}
