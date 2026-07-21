package search

import (
	"testing"
)

func TestParseQuery(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantText string
		wantKind []string
		wantLang []string
		wantPath []string
		wantName []string
	}{
		{
			name:     "plain text",
			input:    "authenticate",
			wantText: "authenticate",
		},
		{
			name:     "kind filter",
			input:    "kind:function authenticate",
			wantText: "authenticate",
			wantKind: []string{"function"},
		},
		{
			name:     "multiple filters",
			input:    "kind:function name:auth path:src/api",
			wantKind: []string{"function"},
			wantName: []string{"auth"},
			wantPath: []string{"src/api"},
		},
		{
			name:     "lang alias",
			input:    "lang:go main",
			wantText: "main",
			wantLang: []string{"go"},
		},
		{
			name:     "language alias",
			input:    "language:python main",
			wantText: "main",
			wantLang: []string{"python"},
		},
		{
			name:     "quoted path",
			input:    `path:"src/my dir" foo`,
			wantText: "foo",
			wantPath: []string{"src/my dir"},
		},
		{
			name:     "unknown field passes through",
			input:    "TODO: fix this",
			wantText: "TODO: fix this",
		},
		{
			name:     "invalid kind ignored",
			input:    "kind:foobar test",
			wantText: "kind:foobar test",
		},
		{
			name:     "empty query",
			input:    "",
			wantText: "",
		},
		{
			name:     "multiple kinds",
			input:    "kind:function kind:method find",
			wantText: "find",
			wantKind: []string{"function", "method"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseQuery(tt.input)
			if got.Text != tt.wantText {
				t.Errorf("Text = %q, want %q", got.Text, tt.wantText)
			}
			if len(got.Kinds) != len(tt.wantKind) {
				t.Errorf("Kinds = %v, want %v", got.Kinds, tt.wantKind)
			}
			if len(got.Languages) != len(tt.wantLang) {
				t.Errorf("Languages = %v, want %v", got.Languages, tt.wantLang)
			}
			if len(got.PathFilters) != len(tt.wantPath) {
				t.Errorf("PathFilters = %v, want %v", got.PathFilters, tt.wantPath)
			}
			if len(got.NameFilters) != len(tt.wantName) {
				t.Errorf("NameFilters = %v, want %v", got.NameFilters, tt.wantName)
			}
		})
	}
}

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

func TestTokenize(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "simple words",
			input: "foo bar baz",
			want:  []string{"foo", "bar", "baz"},
		},
		{
			name:  "quoted string",
			input: `path:"src/my dir" foo`,
			want:  []string{"path:\"src/my dir\"", "foo"},
		},
		{
			name:  "multi-byte with quote",
			input: "日本語 kind:\"テスト\" hello",
			want:  []string{"日本語", "kind:\"テスト\"", "hello"},
		},
		{
			name:  "unclosed quote consumes rest",
			input: `path:"unclosed foo`,
			want:  []string{"path:\"unclosed foo"},
		},
		{
			name:  "unclosed quote only",
			input: `path:"hello`,
			want:  []string{"path:\"hello"},
		},
		{
			name:  "emoji in quoted",
			input: `name:"🚀 launch" kind:function`,
			want:  []string{"name:\"🚀 launch\"", "kind:function"},
		},
		{
			name:  "empty",
			input: "",
			want:  nil,
		},
		{
			name:  "whitespace only",
			input: "   ",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenize(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("tokenize(%q) = %v (len=%d), want %v (len=%d)", tt.input, got, len(got), tt.want, len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("tokenize(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
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
