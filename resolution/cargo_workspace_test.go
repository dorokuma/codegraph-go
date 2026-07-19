package resolution_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/db"
	"github.com/dorokuma/codegraph-go/extraction"
	"github.com/dorokuma/codegraph-go/resolution"
)

func TestLoadCargoWorkspaceMembers(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearCargoCache()
	copyTree(t, filepath.Join("..", "testdata", "parity", "cargo"), dir)

	ws := resolution.LoadCargoWorkspace(dir)
	if ws == nil {
		t.Fatal("expected cargo workspace")
	}
	// hyphen and underscore aliases
	if ws.ByName["demo-core"] == "" && ws.ByName["demo_core"] == "" {
		t.Fatalf("missing demo-core: %+v", ws.ByName)
	}
	core := ws.ByName["demo_core"]
	if core == "" {
		core = ws.ByName["demo-core"]
	}
	if core != "crates/core" {
		t.Fatalf("demo-core dir = %q", core)
	}
	if ws.ByName["demo_app"] != "app" && ws.ByName["demo-app"] != "app" {
		t.Fatalf("demo-app dir missing: %+v", ws.ByName)
	}
}

func TestResolveCargoImportPaths(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearCargoCache()
	copyTree(t, filepath.Join("..", "testdata", "parity", "cargo"), dir)

	from := filepath.Join(dir, "app", "src", "main.rs")
	lib := filepath.Join(dir, "crates", "core", "src", "lib.rs")

	// bare crate
	files := resolution.ResolveImportPath(dir, from, "demo_core", "rust")
	if !containsPath(files, lib) {
		t.Fatalf("demo_core → %v, want %s", files, lib)
	}
	// use path ending in symbol
	files = resolution.ResolveImportPath(dir, from, "demo_core::greet", "rust")
	if !containsPath(files, lib) {
		t.Fatalf("demo_core::greet → %v, want %s", files, lib)
	}
	// hyphen form from Cargo.toml name
	files = resolution.ResolveImportPath(dir, from, "demo-core", "rust")
	if !containsPath(files, lib) {
		t.Fatalf("demo-core → %v", files)
	}
}

func TestParityCargoWorkspaceCall(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearCargoCache()
	copyTree(t, filepath.Join("..", "testdata", "parity", "cargo"), dir)

	database, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	orch := extraction.NewOrchestrator(database, dir)
	if _, _, err := orch.IndexAll(); err != nil {
		t.Fatal(err)
	}

	// Symbols exist
	greet, err := database.GetNodeByName("greet")
	if err != nil || len(greet) == 0 {
		t.Fatalf("greet missing: %v", err)
	}
	run, err := database.GetNodeByName("run")
	if err != nil || len(run) == 0 {
		t.Fatalf("run missing: %v", err)
	}

	assertGraphCall(t, database, "run", "greet")
}

func TestCargoGlobMembersOnly(t *testing.T) {
	dir := t.TempDir()
	resolution.ClearCargoCache()
	// workspace with only glob
	os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(`[workspace]
members = ["crates/*"]
`), 0o644)
	os.MkdirAll(filepath.Join(dir, "crates", "alpha", "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "crates", "alpha", "Cargo.toml"), []byte(`[package]
name = "alpha-lib"
version = "0.1.0"
`), 0o644)
	os.WriteFile(filepath.Join(dir, "crates", "alpha", "src", "lib.rs"), []byte(`pub fn a() {}`), 0o644)

	ws := resolution.LoadCargoWorkspace(dir)
	if ws == nil || ws.ByName["alpha_lib"] != "crates/alpha" {
		t.Fatalf("glob member map = %+v", ws)
	}
}
