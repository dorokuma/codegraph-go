package db

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// parkTxTestIndex builds a two-file index: callers A1/A2 in a.go, targets
// T1/T2 in b.go, with calls edges a.go -> b.go on both targets.
func parkTxTestIndex(t *testing.T) (*DB, [2]int64, [2]int64) {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "idx"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	a1, err := database.UpsertNode(&Node{Kind: KindFunction, Name: "A1", File: "a.go", Line: 1, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := database.UpsertNode(&Node{Kind: KindFunction, Name: "A2", File: "a.go", Line: 2, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	t1, err := database.UpsertNode(&Node{Kind: KindFunction, Name: "T1", File: "b.go", Line: 10, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	t2, err := database.UpsertNode(&Node{Kind: KindFunction, Name: "T2", File: "b.go", Line: 20, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range [][2]int64{{a1, t1}, {a1, t2}, {a2, t2}} {
		if _, err := database.UpsertEdge(&Edge{SourceID: e[0], TargetID: e[1], Kind: EdgeCalls, File: "a.go", Line: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return database, [2]int64{a1, a2}, [2]int64{t1, t2}
}

// TestParkInboundRefsForFileAtomicRollback verifies the single-transaction
// parking contract: a failure in the middle of the park rolls back the whole
// batch, so a partial park can never linger (the per-ref autocommit form
// used to leave already-parked refs behind). The sabotage trigger aborts the
// insert of the SECOND parked ref only.
func TestParkInboundRefsForFileAtomicRollback(t *testing.T) {
	database, _, _ := parkTxTestIndex(t)

	if _, err := database.conn.Exec(`
		CREATE TRIGGER park_boom BEFORE INSERT ON unresolved_refs
		WHEN NEW.reference_name = 'T2'
		BEGIN
			SELECT RAISE(ABORT, 'boom');
		END
	`); err != nil {
		t.Fatal(err)
	}

	if err := database.ParkInboundRefsForFile("b.go"); err == nil {
		t.Fatal("expected the sabotaged park to fail")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Rolled back: not even the first (unsabotaged) ref may survive.
	if n, _ := database.CountUnresolvedRefs(""); n != 0 {
		t.Fatalf("partial park survived the failure: %d rows", n)
	}

	// Without the sabotage the same call parks both refs.
	if _, err := database.conn.Exec(`DROP TRIGGER park_boom`); err != nil {
		t.Fatal(err)
	}
	if err := database.ParkInboundRefsForFile("b.go"); err != nil {
		t.Fatalf("clean park failed: %v", err)
	}
	refs, err := database.ListUnresolvedRefs("", "pending")
	if err != nil {
		t.Fatal(err)
	}
	// A1->T1, A1->T2, A2->T2: three parked refs.
	if len(refs) != 3 {
		t.Fatalf("want 3 parked refs, got %d: %v", len(refs), refs)
	}
	for _, r := range refs {
		if r.FilePath != "a.go" {
			t.Fatalf("parked ref file_path=%q, want the SOURCE file a.go", r.FilePath)
		}
	}
}

// TestParkInboundRefsConcurrentWithReplace hammers the M7 race window: parking
// b.go's inbound refs while a.go is repeatedly replaced (its nodes and their
// inbound edges CASCADE-die). Parking runs in one transaction under the write
// lock, so no insert may ever hit a from_node deleted by the concurrent
// replace — the FOREIGN KEY failure the per-ref autocommit form allowed.
func TestParkInboundRefsConcurrentWithReplace(t *testing.T) {
	database, _, targets := parkTxTestIndex(t)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 64)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			fr := &FileRecord{Path: "a.go", ContentHash: "h", Language: "go", NodeCount: 2}
			_, err := database.ReplaceFileIndex("a.go", []Node{
				{Kind: KindFunction, Name: "A1", File: "a.go", Line: 1, Language: "go"},
				{Kind: KindFunction, Name: "A2", File: "a.go", Line: 2, Language: "go"},
			}, []Edge{
				{SourceID: -1, TargetID: targets[0], Kind: EdgeCalls, File: "a.go", Line: 1},
				{SourceID: -2, TargetID: targets[1], Kind: EdgeCalls, File: "a.go", Line: 3},
			}, nil, fr)
			if err != nil {
				select {
				case errs <- err:
				default:
				}
				return
			}
		}
	}()

	var raced string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := database.ParkInboundRefsForFile("b.go"); err != nil {
			if strings.Contains(err.Error(), "FOREIGN KEY") {
				raced = err.Error()
			}
			select {
			case errs <- err:
			default:
			}
			break
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent writer failed: %v", err)
	}
	if raced != "" {
		t.Fatalf("park raced a concurrent cascade delete: %s", raced)
	}
}
