package search

import (
	"testing"
)

func TestSplitIdentifierSegments(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"UserService", []string{"user", "service"}},
		{"getUserByID", []string{"get", "user", "by", "id"}},
		{"parse_query_parser", []string{"parse", "query", "parser"}},
		{"XMLParser", []string{"xml", "parser"}},
		{"HTTPSConnection", []string{"https", "connection"}},
		{"simple", []string{"simple"}},
		{"", nil},
		{"A", []string{"a"}},
		{"getHTTPResponse", []string{"get", "http", "response"}},
		{"$state", []string{"state"}},
		// Multi-byte camelCase: caféBar should split correctly
		{"caféBar", []string{"café", "bar"}},
		{"überCool", []string{"über", "cool"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := SplitIdentifierSegments(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("SplitIdentifierSegments(%q) = %v, want %v", tt.input, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("SplitIdentifierSegments(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestBoundedEditDistance(t *testing.T) {
	tests := []struct {
		a, b    string
		maxDist int
		want    int
	}{
		{"kitten", "kitten", 2, 0},
		{"kitten", "sitting", 2, 3}, // distance=3, > maxDist=2 → returns 3
		{"kitten", "sitting", 5, 3},
		{"", "abc", 3, 3},
		{"abc", "", 3, 3},
		{"abc", "abc", 1, 0},
		{"ab", "cd", 1, 2}, // distance=2, > maxDist=1 → returns maxDist+1=2
		{"ab", "cd", 3, 2},
	}

	for _, tt := range tests {
		got := BoundedEditDistance(tt.a, tt.b, tt.maxDist)
		if got != tt.want {
			t.Errorf("BoundedEditDistance(%q, %q, %d) = %d, want %d", tt.a, tt.b, tt.maxDist, got, tt.want)
		}
	}
}

func TestFuzzyMatchScore(t *testing.T) {
	tests := []struct {
		name    string
		query   []string
		target  []string
		wantMin int // minimum expected score
	}{
		{"exact match", []string{"user"}, []string{"user"}, 10},
		{"partial", []string{"user"}, []string{"user", "service"}, 10},
		{"no match", []string{"foo"}, []string{"user"}, 0},
		{"multi segment", []string{"user", "service"}, []string{"user", "service"}, 30}, // 10+10+5+20
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FuzzyMatchScore(tt.query, tt.target)
			if got < tt.wantMin {
				t.Errorf("FuzzyMatchScore(%v, %v) = %d, want >= %d", tt.query, tt.target, got, tt.wantMin)
			}
		})
	}
}
