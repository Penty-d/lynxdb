package rest

import (
	"github.com/lynxbase/lynxdb/pkg/langdetect"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
)

// langDetectResult holds the outcome of language detection.
type langDetectResult struct {
	// Language is the resolved language ("lynxflow" or "spl2").
	Language QueryLanguage
	// Explicit is true when the caller specified the language explicitly.
	Explicit bool
	// DetectNotice is non-empty when detection was used (not explicit) and
	// provides a human-readable notice about the detection result.
	DetectNotice string
}

// detectQueryLanguage resolves the language for a query.
// This delegates to the shared langdetect package and converts the result
// to the REST-layer type.
func detectQueryLanguage(query string, explicitLang string) langDetectResult {
	r := langdetect.Detect(query, explicitLang)
	return langDetectResult{
		Language:     QueryLanguage(r.Language),
		Explicit:     r.Explicit,
		DetectNotice: r.DetectNotice,
	}
}

// hasErrorDiag reports whether any diagnostic has error severity.
func hasErrorDiag(diags []parser.Diag) bool {
	return langdetect.HasErrorDiag(diags)
}

// validateExplicitLanguage returns an error message if the language value is
// invalid. Returns "" for valid or absent values.
func validateExplicitLanguage(lang string) string {
	return langdetect.ValidateExplicitLanguage(lang)
}
