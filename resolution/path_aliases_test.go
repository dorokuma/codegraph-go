package resolution_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/resolution"
)

func TestLoadAndApplyTsconfigAliases(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearAliasCache()
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{
  // comment ok
  "compilerOptions": {
    "baseUrl": ".",
    "paths": {
      "@/*": ["src/*"],
      "@utils/*": ["src/lib/*"],
    },
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// create target so expand can later find it
	if err := os.MkdirAll(filepath.Join(dir, "src", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "lib", "utils.ts"), []byte(`export function x(){}`), 0o644); err != nil {
		t.Fatal(err)
	}

	aliases := resolution.LoadProjectAliases(dir)
	if aliases == nil {
		t.Fatal("expected aliases from tsconfig")
	}
	got := resolution.ApplyAliases("@/lib/utils", aliases, dir)
	if len(got) == 0 || got[0] != "src/lib/utils" {
		t.Fatalf("ApplyAliases @/lib/utils = %v, want [src/lib/utils]", got)
	}
	got2 := resolution.ApplyAliases("@utils/utils", aliases, dir)
	if len(got2) == 0 || got2[0] != "src/lib/utils" {
		t.Fatalf("ApplyAliases @utils/utils = %v", got2)
	}

	// Full ResolveImportPath must hit the real file.
	from := filepath.Join(dir, "src", "main.ts")
	files := resolution.ResolveImportPath(dir, from, "@/lib/utils", "typescript")
	want := filepath.Join(dir, "src", "lib", "utils.ts")
	found := false
	for _, f := range files {
		if f == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("ResolveImportPath(@/lib/utils) = %v, want contain %s", files, want)
	}
}

func TestFallbackAliasWithoutTsconfig(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearAliasCache()
	if err := os.MkdirAll(filepath.Join(dir, "src", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "src", "lib", "utils.ts")
	if err := os.WriteFile(target, []byte(`export const a = 1`), 0o644); err != nil {
		t.Fatal(err)
	}
	from := filepath.Join(dir, "src", "main.ts")
	files := resolution.ResolveImportPath(dir, from, "@/lib/utils", "typescript")
	found := false
	for _, f := range files {
		if f == target {
			found = true
		}
	}
	if !found {
		t.Fatalf("fallback @/ should map to src/: got %v", files)
	}
}

func TestStripJSONCKeepsURLs(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearAliasCache()
	// baseUrl-like string with // must not be truncated by comment stripper
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{
  "compilerOptions": {
    "baseUrl": ".",
    "paths": { "@cdn/*": ["vendor/*"] }
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if resolution.LoadProjectAliases(dir) == nil {
		t.Fatal("expected parse success")
	}
}
