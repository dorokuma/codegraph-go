package extraction

import (
	"testing"
)

func TestSplitRustUsePaths(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"std::io", []string{"std::io"}},
		{"std::io::{Read, Write}", []string{"std::io::Read", "std::io::Write"}},
		{"crate::{a, b, c}", []string{"crate::a", "crate::b", "crate::c"}},
		{"serde::{self, de}", []string{"serde", "serde::de"}},
		{"foo::bar::{}", []string{"foo::bar"}},
		{"simple", []string{"simple"}},
		{"", nil},
	}
	for _, tt := range tests {
		got := splitRustUsePaths(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("splitRustUsePaths(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitRustUsePaths(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestSimplifyHandlerNameOrchestrator(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"listUsers", "listUsers"},
		{"UsersController@index", "index"},
		{"pkg.Handler", "Handler"},
		{"(User).Create", "Create"},
		{"handler()", "handler"},
		{"  trimmed  ", "trimmed"},
		{"", ""},
	}
	for _, tt := range tests {
		got := simplifyHandlerName(tt.input)
		if got != tt.want {
			t.Errorf("simplifyHandlerName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBareRank(t *testing.T) {
	if bareRank("function") != 3 {
		t.Errorf("bareRank(function) = %d, want 3", bareRank("function"))
	}
	if bareRank("method") != 3 {
		t.Errorf("bareRank(method) = %d, want 3", bareRank("method"))
	}
	if bareRank("class") != 2 {
		t.Errorf("bareRank(class) = %d, want 2", bareRank("class"))
	}
	if bareRank("route") != 1 {
		t.Errorf("bareRank(route) = %d, want 1", bareRank("route"))
	}
	if bareRank("variable") != 0 {
		t.Errorf("bareRank(variable) = %d, want 0", bareRank("variable"))
	}
}

func TestSplitNameLineKey(t *testing.T) {
	tests := []struct {
		key      string
		wantName string
		wantLine int
		wantOk   bool
	}{
		{"foo:10", "foo", 10, true},
		{"myFunc:42", "myFunc", 42, true},
		{"pkg.Type:1", "pkg.Type", 1, true},
		{"", "", 0, false},
		{"noline", "", 0, false},
		{":0", "", 0, true},
	}
	for _, tt := range tests {
		name, line, ok := splitNameLineKey(tt.key)
		if ok != tt.wantOk || name != tt.wantName || line != tt.wantLine {
			t.Errorf("splitNameLineKey(%q) = (%q, %d, %v), want (%q, %d, %v)",
				tt.key, name, line, ok, tt.wantName, tt.wantLine, tt.wantOk)
		}
	}
}

func TestShouldParkRef(t *testing.T) {
	// ShouldParkRef takes a db and a name. Without a db, it should handle nil gracefully.
	// We can't easily test this without a db, but we can test the edge cases.
	// For now, just verify it doesn't panic with empty names.
	if ShouldParkRef(nil, "") {
		t.Error("ShouldParkRef(nil, \"\") should be false")
	}
}
