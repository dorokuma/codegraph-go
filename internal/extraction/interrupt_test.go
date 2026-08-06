package extraction

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

func writeGoFiles(t *testing.T, root string, n int) []string {
	t.Helper()
	var paths []string
	for i := 0; i < n; i++ {
		p := filepath.Join(root, fmt.Sprintf("f%02d.go", i))
		if err := os.WriteFile(p, []byte("package p\nfunc F() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}

// TestIndexAllInterruptedBeforeStart: with done already closed, IndexAll must
// return ErrIndexInterrupted (errors.Is hit), not a clean nil — the caller
// must never mistake an interrupted pass for a completed one.
func TestIndexAllInterruptedBeforeStart(t *testing.T) {
	root := t.TempDir()
	writeGoFiles(t, root, 20)
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	orch := NewOrchestrator(database, root)
	done := make(chan struct{})
	orch.SetDone(done)
	close(done)

	files, nodes, err := orch.IndexAll()
	if !errors.Is(err, ErrIndexInterrupted) {
		t.Fatalf("IndexAll after shutdown: err=%v (want ErrIndexInterrupted), files=%d nodes=%d", err, files, nodes)
	}
	if files >= 20 {
		t.Fatalf("IndexAll must not index the whole workspace after shutdown: files=%d", files)
	}
}

// TestIndexAllInterruptedMidPass: the shutdown channel closes while jobs are
// being indexed (injected via the readFileFn seam — the 5th file read closes
// done). IndexAll must return ErrIndexInterrupted with partial results, not
// nil and not a full pass.
func TestIndexAllInterruptedMidPass(t *testing.T) {
	root := t.TempDir()
	writeGoFiles(t, root, 40)
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	orch := NewOrchestrator(database, root)
	done := make(chan struct{})
	orch.SetDone(done)

	var reads atomic.Int32
	orch.readFileFn = func(path string) ([]byte, error) {
		if reads.Add(1) == 5 {
			close(done) // interrupt the pass mid-flight
		}
		return os.ReadFile(path)
	}

	files, _, err := orch.IndexAll()
	if !errors.Is(err, ErrIndexInterrupted) {
		t.Fatalf("IndexAll interrupted mid-pass: err=%v (want ErrIndexInterrupted), files=%d", err, files)
	}
	if files == 0 || files >= 40 {
		t.Fatalf("expected partial results (0 < files < 40), got files=%d", files)
	}
}

// TestIndexAllWithProgressInterrupted: same sentinel contract for the
// progress variant.
func TestIndexAllWithProgressInterrupted(t *testing.T) {
	root := t.TempDir()
	writeGoFiles(t, root, 10)
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	orch := NewOrchestrator(database, root)
	done := make(chan struct{})
	orch.SetDone(done)
	close(done)

	if _, _, err := orch.IndexAllWithProgress(nil); !errors.Is(err, ErrIndexInterrupted) {
		t.Fatalf("IndexAllWithProgress after shutdown: err=%v (want ErrIndexInterrupted)", err)
	}
}

// TestIndexChangesInterrupted: the batch loop stops at the first interrupt
// check and returns ErrIndexInterrupted, keeping whatever was written.
func TestIndexChangesInterrupted(t *testing.T) {
	root := t.TempDir()
	files := writeGoFiles(t, root, 10)
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	orch := NewOrchestrator(database, root)
	done := make(chan struct{})
	orch.SetDone(done)
	close(done)

	if _, _, err := orch.IndexChanges(files); !errors.Is(err, ErrIndexInterrupted) {
		t.Fatalf("IndexChanges after shutdown: err=%v (want ErrIndexInterrupted)", err)
	}
}

// TestRebuildAllInterruptedSkipsSchemaRevision: an interrupted RebuildAll
// (M1 regression) must NOT mark the schema revision — NeedsRebuild must stay
// true so the next startup re-runs the full rebuild instead of trusting the
// wiped/half index. Control: a complete RebuildAll does mark the revision.
func TestRebuildAllInterruptedSkipsSchemaRevision(t *testing.T) {
	root := t.TempDir()
	writeGoFiles(t, root, 20)
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// A fresh DB has no revision record → NeedsRebuild is true (the exact
	// state in which the server calls RebuildAll).
	rebuild, _, err := database.NeedsRebuild()
	if err != nil {
		t.Fatal(err)
	}
	if !rebuild {
		t.Fatal("fresh DB must need a rebuild")
	}

	orch := NewOrchestrator(database, root)
	done := make(chan struct{})
	orch.SetDone(done)
	close(done)

	if _, _, err := orch.RebuildAll(); !errors.Is(err, ErrIndexInterrupted) {
		t.Fatalf("RebuildAll after shutdown: err=%v (want ErrIndexInterrupted)", err)
	}
	rebuild, _, err = database.NeedsRebuild()
	if err != nil {
		t.Fatal(err)
	}
	if !rebuild {
		t.Fatal("NeedsRebuild must remain true after an interrupted RebuildAll — the schema revision must not be marked")
	}

	// Control: a complete RebuildAll marks the revision.
	orch2 := NewOrchestrator(database, root) // no done channel
	if _, _, err := orch2.RebuildAll(); err != nil {
		t.Fatalf("complete RebuildAll: %v", err)
	}
	rebuild, _, err = database.NeedsRebuild()
	if err != nil {
		t.Fatal(err)
	}
	if rebuild {
		t.Fatal("complete RebuildAll must mark the schema revision (NeedsRebuild=false)")
	}
}
