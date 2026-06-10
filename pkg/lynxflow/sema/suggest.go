package sema

import (
	"fmt"
	"sort"
	"strings"
)

// maxLevenshteinDistance is the maximum edit distance for a did-you-mean
// suggestion. Fields further away are not suggested.
const maxLevenshteinDistance = 3

// didYouMean returns a "did you mean X?" suggestion string for the given
// name against a list of candidates, using Levenshtein distance and
// substring containment as a fallback.
// Returns empty string if no close match is found.
func didYouMean(name string, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}

	nameLower := strings.ToLower(name)

	type match struct {
		name string
		dist int
	}
	var matches []match

	for _, c := range candidates {
		cLower := strings.ToLower(c)
		d := levenshtein(nameLower, cLower)
		if d <= maxLevenshteinDistance && d > 0 {
			matches = append(matches, match{name: c, dist: d})
		}
	}

	// Fallback: if no Levenshtein match, try substring containment.
	// e.g., "timestamp" contains "time" which matches "_time".
	if len(matches) == 0 {
		// Extract base word from name (strip common prefixes/suffixes).
		// Check if any candidate's base is contained in the name or vice versa.
		for _, c := range candidates {
			cLower := strings.ToLower(c)
			cBase := strings.TrimLeft(cLower, "_")
			nBase := strings.TrimLeft(nameLower, "_")
			if len(cBase) >= 3 && len(nBase) >= 3 {
				if strings.Contains(nBase, cBase) || strings.Contains(cBase, nBase) {
					// Use a synthetic distance of maxLevenshteinDistance + 1
					// so Levenshtein matches are always preferred.
					matches = append(matches, match{name: c, dist: maxLevenshteinDistance + 1})
				}
			}
		}
	}

	if len(matches) == 0 {
		return ""
	}

	// Sort by distance, then alphabetically.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].dist != matches[j].dist {
			return matches[i].dist < matches[j].dist
		}
		return matches[i].name < matches[j].name
	})

	return fmt.Sprintf("did you mean '%s'?", matches[0].name)
}

// levenshtein computes the edit distance between two strings.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	// Use two rows for space efficiency.
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)

	for j := 0; j <= lb; j++ {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(
				prev[j]+1,      // deletion
				curr[j-1]+1,    // insertion
				prev[j-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}

	return prev[lb]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
