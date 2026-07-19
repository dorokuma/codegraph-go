package search

import (
	"strings"
	"unicode"
)

// SplitIdentifierSegments splits a camelCase or snake_case identifier into
// lowercase segments for fuzzy matching.
//
// Examples:
//
//	"UserService" → ["user", "service"]
//	"getUserByID" → ["get", "user", "by", "id"]
//	"parse_query_parser" → ["parse", "query", "parser"]
//	"XMLParser" → ["xml", "parser"]
//	"HTTPSConnection" → ["https", "connection"]
func SplitIdentifierSegments(name string) []string {
	if name == "" {
		return nil
	}

	// Normalize separators to spaces.
	normalized := strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == '.' || r == '$' {
			return ' '
		}
		return r
	}, name)

	// Split camelCase: insert space before uppercase that follows lowercase.
	var buf strings.Builder
	for i, r := range normalized {
		if i > 0 && unicode.IsUpper(r) {
			prev := rune(normalized[i-1])
			if unicode.IsLower(prev) {
				buf.WriteRune(' ')
			}
		}
		buf.WriteRune(r)
	}

	// Split consecutive uppercase at boundary: "XMLParser" → "XML Parser"
	raw := buf.String()
	var result strings.Builder
	runes2 := []rune(raw)
	for i, r := range runes2 {
		if i > 0 && i+1 < len(runes2) && unicode.IsUpper(r) && unicode.IsUpper(runes2[i-1]) && unicode.IsLower(runes2[i+1]) {
			result.WriteRune(' ')
		}
		result.WriteRune(r)
	}

	parts := strings.Fields(strings.ToLower(result.String()))
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// FuzzyMatchScore returns a score for how well querySegments match against
// nameSegments. Higher is better. 0 means no match.
// This is a simple subsequence-based scorer.
func FuzzyMatchScore(querySegments, nameSegments []string) int {
	if len(querySegments) == 0 || len(nameSegments) == 0 {
		return 0
	}

	score := 0
	qi := 0
	for ni := 0; ni < len(nameSegments) && qi < len(querySegments); ni++ {
		if nameSegments[ni] == querySegments[qi] {
			score += 10
			// Bonus for consecutive match
			if qi > 0 && ni > 0 {
				score += 5
			}
			qi++
		}
	}

	// All query segments matched
	if qi == len(querySegments) {
		// Bonus for exact length match
		if len(querySegments) == len(nameSegments) {
			score += 20
		}
		return score
	}

	return 0 // not all segments matched
}
