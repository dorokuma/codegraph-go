package resolution_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/resolution"
)

func TestLoadGoModuleAndReplace(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearGoModuleCache()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(`module example.com/demo

go 1.22

replace example.com/replaced => ./pkgb
replace (
	example.com/other => ../other
)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkga"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkgb"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, "pkga", "a.go")
	b := filepath.Join(dir, "pkgb", "b.go")
	os.WriteFile(a, []byte("package pkga\nfunc Helper(){}\n"), 0o644)
	os.WriteFile(b, []byte("package pkgb\nfunc Replaced(){}\n"), 0o644)

	mod := resolution.LoadGoModule(dir)
	if mod == nil || mod.ModulePath != "example.com/demo" {
		t.Fatalf("LoadGoModule = %+v", mod)
	}
	pkgDir := resolution.ResolveGoImport(mod, "example.com/demo/pkga")
	if pkgDir != filepath.Join(dir, "pkga") {
		t.Fatalf("module subpkg = %q", pkgDir)
	}
	repl := resolution.ResolveGoImport(mod, "example.com/replaced")
	if repl != filepath.Join(dir, "pkgb") {
		t.Fatalf("replace = %q want pkgb", repl)
	}

	from := filepath.Join(dir, "main.go")
	files := resolution.ResolveImportPath(dir, from, "example.com/demo/pkga", "go")
	found := false
	for _, f := range files {
		if f == a {
			found = true
		}
	}
	if !found {
		t.Fatalf("ResolveImportPath go module = %v, want %s", files, a)
	}
	files = resolution.ResolveImportPath(dir, from, "example.com/replaced", "go")
	found = false
	for _, f := range files {
		if f == b {
			found = true
		}
	}
	if !found {
		t.Fatalf("ResolveImportPath go replace = %v, want %s", files, b)
	}
}
