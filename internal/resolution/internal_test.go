package resolution

import (
	"testing"
)

func TestLRUCacheEviction(t *testing.T) {
	lru := newLRUCache(3)

	// Fill cache
	lru.put("a", "1")
	lru.put("b", "2")
	lru.put("c", "3")
	if lru.len() != 3 {
		t.Fatalf("len after 3 puts: got %d want 3", lru.len())
	}

	// Access "a" to make it most recent
	v, ok := lru.get("a")
	if !ok || v != "1" {
		t.Fatalf("get a: ok=%v v=%q", ok, v)
	}

	// Add "d" — should evict "b" (oldest, since "a" was touched)
	lru.put("d", "4")
	if lru.len() != 3 {
		t.Fatalf("len after 4th put: got %d want 3", lru.len())
	}
	if _, ok := lru.get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if v, ok := lru.get("a"); !ok || v != "1" {
		t.Fatalf("a should still be present: ok=%v v=%q", ok, v)
	}
	if v, ok := lru.get("c"); !ok || v != "3" {
		t.Fatalf("c should still be present: ok=%v v=%q", ok, v)
	}
	if v, ok := lru.get("d"); !ok || v != "4" {
		t.Fatalf("d should be present: ok=%v v=%q", ok, v)
	}

	// Overwrite existing key should not evict
	lru.put("a", "11")
	if lru.len() != 3 {
		t.Fatalf("len after overwrite: got %d want 3", lru.len())
	}
	if v, _ := lru.get("a"); v != "11" {
		t.Fatalf("a after overwrite: got %q want 11", v)
	}
}

func TestLRUCachePutEmpty(t *testing.T) {
	lru := newLRUCache(2)
	lru.put("x", "")
	v, ok := lru.get("x")
	if !ok || v != "" {
		t.Fatalf("empty value: ok=%v v=%q", ok, v)
	}
}

func TestSpecMatchesFile(t *testing.T) {
	tests := []struct {
		file, spec string
		want       bool
	}{
		// Exact match
		{"src/util.ts", "src/util.ts", true},
		// spec as first segment
		{"components/Button.tsx", "components", true},
		// spec as last segment (directory)
		{"src/components/Button.tsx", "components", true},
		// spec as middle segment
		{"src/components/ui/Button.tsx", "components", true},
		// basename match (file == spec with no dir)
		{"util.ts", "util.ts", true},
		// SHOULD NOT match: substring without boundaries
		{"src/mycomponents/Button.tsx", "components", false},
		{"src/components_old/Button.tsx", "components", false},
		{"src/util_extra.ts", "util", false},
		// spec with slashes
		{"src/lib/utils.ts", "lib/utils", false}, // "lib/utils" not a single segment
		{"lib/utils/index.ts", "lib/utils", true}, // starts with "lib/utils/"
		{"src/lib/utils.ts", "lib/utils", false},  // does not start with "lib/utils/"
	}
	for _, tc := range tests {
		got := specMatchesFile(tc.file, tc.spec)
		if got != tc.want {
			t.Errorf("specMatchesFile(%q, %q) = %v, want %v", tc.file, tc.spec, got, tc.want)
		}
	}
}
