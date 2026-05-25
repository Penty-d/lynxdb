package index

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Tokenize splits text into lowercase tokens suitable for full-text search.
// It handles whitespace + punctuation splitting. Tokens are substrings of the
// lowercased input — no Builder or rune-slice allocations on the hot path.
func Tokenize(text string) []string {
	tokens := make([]string, 0, 16)
	start := -1 // start index of current token in text, -1 = no active token
	needsLower := false

	for i := 0; i < len(text); {
		b := text[i]
		if b < utf8.RuneSelf {
			// ASCII fast path (covers >99% of log data).
			if isAlnum(b) {
				if start < 0 {
					start = i
					needsLower = false
				}
				if b >= 'A' && b <= 'Z' {
					needsLower = true
				}
				i++
			} else {
				// Any non-alnum ASCII char is a token boundary
				// (includes ':', '-', '_', whitespace, punctuation).
				if start >= 0 {
					tokens = appendToken(tokens, text[start:i], needsLower)
					start = -1
				}
				i++
			}
		} else {
			// Non-ASCII: decode rune and classify.
			r, size := utf8.DecodeRuneInString(text[i:])
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				if start < 0 {
					start = i
					needsLower = false
				}
				if unicode.IsUpper(r) {
					needsLower = true
				}
				i += size
			} else {
				if start >= 0 {
					tokens = appendToken(tokens, text[start:i], needsLower)
					start = -1
				}
				i += size
			}
		}
	}
	if start >= 0 {
		tokens = appendToken(tokens, text[start:], needsLower)
	}

	return tokens
}

// isAlnum returns true for ASCII letters and digits.
func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

func appendToken(tokens []string, token string, needsLower bool) []string {
	if needsLower {
		token = strings.ToLower(token)
	}
	return append(tokens, token)
}

// TokenizeUnique returns deduplicated tokens in stable order.
func TokenizeUnique(text string) []string {
	tokens := Tokenize(text)
	seen := make(map[string]bool, len(tokens))
	unique := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if !seen[t] {
			seen[t] = true
			unique = append(unique, t)
		}
	}

	return unique
}
