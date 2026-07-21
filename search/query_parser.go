package search

import (
	"strings"
	"unicode"
)

// ParsedQuery is a structured search query with field filters + free text.
// Ported from official search/query-parser.ts.
type ParsedQuery struct {
	// Text is the free-text portion for FTS. May be empty.
	Text string
	// Kinds filters by node kind (OR'd). Empty = all kinds.
	Kinds []string
	// Languages filters by language (OR'd). Empty = all languages.
	Languages []string
	// PathFilters filters by case-insensitive substring of file path (OR'd).
	PathFilters []string
	// NameFilters filters by case-insensitive substring of node name (OR'd).
	NameFilters []string
}

// validKinds is the set of recognized node kinds.
var validKinds = map[string]bool{
	"function": true, "method": true, "class": true, "interface": true,
	"struct": true, "type": true, "variable": true, "constant": true,
	"file": true, "module": true, "route": true, "component": true,
	"constructor": true, "enum": true, "trait": true, "protocol": true,
	"namespace": true, "foreign_function": true,
}

// validLanguages is the set of recognized languages.
var validLanguages = map[string]bool{
	"go": true, "typescript": true, "javascript": true, "python": true,
	"rust": true, "java": true, "csharp": true, "ruby": true, "php": true,
	"c": true, "cpp": true, "swift": true, "kotlin": true, "scala": true,
	"dart": true, "lua": true, "luau": true, "r": true, "objective-c": true,
	"svelte": true, "vue": true, "astro": true, "liquid": true, "pascal": true,
	"nix": true, "erlang": true, "cobol": true, "solidity": true,
	"terraform": true, "vbnet": true, "arkts": true, "cfml": true,
}

// ParseQuery parses a raw query string into structured filters + free text.
//
// Recognized fields (case-insensitive):
//   kind:    function|method|class|...
//   lang:    typescript|python|go|...  (alias: language:)
//   path:    case-insensitive substring of file_path
//   name:    case-insensitive substring of symbol name
//
// Unknown field prefixes are passed through as plain text.
// Quoting: kind:function path:"src/my dir" works.
func ParseQuery(raw string) ParsedQuery {
	out := ParsedQuery{}

	tokens := tokenize(raw)
	var textParts []string

	for _, tok := range tokens {
		colon := strings.Index(tok, ":")
		if colon <= 0 || colon == len(tok)-1 {
			textParts = append(textParts, tok)
			continue
		}
		key := strings.ToLower(tok[:colon])
		valueRaw := unquote(tok[colon+1:])
		if valueRaw == "" {
			textParts = append(textParts, tok)
			continue
		}

		switch key {
		case "kind":
			if validKinds[valueRaw] {
				out.Kinds = append(out.Kinds, valueRaw)
			} else {
				textParts = append(textParts, tok)
			}
		case "lang", "language":
			lower := strings.ToLower(valueRaw)
			if validLanguages[lower] {
				out.Languages = append(out.Languages, lower)
			} else {
				textParts = append(textParts, tok)
			}
		case "path":
			out.PathFilters = append(out.PathFilters, valueRaw)
		case "name":
			out.NameFilters = append(out.NameFilters, valueRaw)
		default:
			textParts = append(textParts, tok)
		}
	}

	out.Text = strings.TrimSpace(strings.Join(textParts, " "))
	return out
}

// tokenize splits on whitespace, preserving quoted spans.
func tokenize(raw string) []string {
	var tokens []string
	i := 0
	runes := []rune(raw)
	for i < len(runes) {
		// skip whitespace
		for i < len(runes) && unicode.IsSpace(runes[i]) {
			i++
		}
		if i >= len(runes) {
			break
		}
		start := i
		for i < len(runes) && !unicode.IsSpace(runes[i]) {
			if runes[i] == '"' {
				// search for closing quote in rune space
				foundClosing := false
				for j := i + 1; j < len(runes); j++ {
					if runes[j] == '"' {
						i = j + 1 // skip past closing quote
						foundClosing = true
						break
					}
				}
				if !foundClosing {
					i = len(runes)
				}
				break
			}
			i++
		}
		tokens = append(tokens, string(runes[start:i]))
	}
	return tokens
}

// unquote strips surrounding double quotes.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
