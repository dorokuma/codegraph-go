package resolution

import (
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// newAbandonTestDB builds an index with one caller node in a.go plus a
// resolvable target (same file, so the unique-callable same-directory
// heuristic fires) and one self-referencing ref that can never resolve:
// CollectCandidates finds the caller itself, the self-filter rejects it, so
// every pass re-attempts and re-fails it — the aging fixture for the
// attempts/abandoned semantics.
func newAbandonTestDB(t *testing.T) (*db.DB, int64) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "idx"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	caller, err := database.UpsertNode(&db.Node{Kind: "function", Name: "callerX", File: "a.go", Line: 1, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpsertNode(&db.Node{Kind: "function", Name: "targetX", File: "a.go", Line: 5, Language: "go"}); err != nil {
		t.Fatal(err)
	}
	for _, rr := range []struct {
		name string
		line int
	}{{"callerX", 2}, {"targetX", 6}} {
		if _, err := database.InsertUnresolvedRef(&db.UnresolvedRef{
			FromNode:      caller,
			ReferenceName: rr.name,
			ReferenceKind: db.EdgeCalls,
			Line:          rr.line,
			FilePath:      "a.go",
			Language:      "go",
			Status:        "pending",
			NameTail:      rr.name,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return database, caller
}

// refState reads the doomed ref (callerX self-reference): its attempts count
// and status. The attempts increments per failed attempt are pinned at the
// SQL level by TestMarkUnresolvedFailedAttemptsAbandon in internal/db; here
// the pass-by-pass aging and status transitions are what matter.
func refState(t *testing.T, database *db.DB) (int, string) {
	t.Helper()
	refs, err := database.ListUnresolvedRefs("", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.ReferenceName == "callerX" {
			attempts, err := database.UnresolvedRefAttempts(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			return attempts, r.Status
		}
	}
	t.Fatal("doomed ref row disappeared")
	return 0, ""
}

// TestResolveAllAbandonsRefAtAttemptCap runs ResolveAll with a shrunken cap:
// failures bump attempts, the cap flips the row to 'abandoned', later passes
// no longer select it (no retries, no failures, resolved counts untouched),
// and a ref parked as abandoned still leaves room for fresh resolutions.
func TestResolveAllAbandonsRefAtAttemptCap(t *testing.T) {
	database, _ := newAbandonTestDB(t)

	oldCap := maxResolveAttempts
	maxResolveAttempts = 3
	t.Cleanup(func() { maxResolveAttempts = oldCap })

	for pass := 1; pass <= 3; pass++ {
		st, err := ResolveAll(database, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		attempts, status := refState(t, database)
		if attempts != pass {
			t.Fatalf("pass %d: attempts=%d", pass, attempts)
		}
		// Pass 1: the pending doomed ref fails (Resolved=1 via the resolvable
		// one). Passes 2-3: the failed-but-retryable doomed ref is re-attempted
		// (Retried=1) and fails again; at the cap it flips to abandoned.
		wantFailed, wantRetried, wantAbandoned := 1, 0, 0
		if pass >= 2 {
			wantRetried = 1
		}
		if pass == 3 {
			wantAbandoned = 1
		}
		if st.Failed != wantFailed || st.Retried != wantRetried || st.Abandoned != wantAbandoned {
			t.Fatalf("pass %d: st=%+v, want failed=%d retried=%d abandoned=%d",
				pass, st, wantFailed, wantRetried, wantAbandoned)
		}
		if pass < 3 && status != "failed" {
			t.Fatalf("pass %d: status=%s, want failed", pass, status)
		}
		if pass == 3 && status != "abandoned" {
			t.Fatalf("pass %d at cap: status=%s, want abandoned", pass, status)
		}
		if pass == 1 && st.Resolved != 1 {
			t.Fatalf("pass 1: Resolved=%d, want 1", st.Resolved)
		}
		if pass > 1 && st.Resolved != 0 {
			t.Fatalf("pass %d: Resolved=%d, want 0", pass, st.Resolved)
		}
	}

	// Post-cap pass: the abandoned ref is invisible to resolution.
	st, err := ResolveAll(database, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if st.Resolved != 0 || st.Failed != 0 || st.Retried != 0 || st.Abandoned != 0 {
		t.Fatalf("post-cap pass must be idle for the abandoned ref, got %+v", st)
	}
	attempts, status := refState(t, database)
	if attempts != 3 || status != "abandoned" {
		t.Fatalf("abandoned row mutated after cap: attempts=%d status=%s", attempts, status)
	}
	if n, _ := database.CountUnresolvedRefs("abandoned"); n != 1 {
		t.Fatalf("abandoned count=%d, want 1 (row kept for audit)", n)
	}
}

// TestResolveForFilesAgesFailedRefs verifies the incremental pass also
// attempts-and-fails previously failed refs: each pass bumps attempts (the
// pre-0.9.12 form skipped re-marking already-failed rows, so they never aged)
// until the cap parks the row as abandoned.
func TestResolveForFilesAgesFailedRefs(t *testing.T) {
	database, _ := newAbandonTestDB(t)

	oldCap := maxResolveAttempts
	maxResolveAttempts = 3
	t.Cleanup(func() { maxResolveAttempts = oldCap })

	// First pass fails both refs (pending): attempts=1 on the doomed one.
	if _, err := ResolveAll(database, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if attempts, _ := refState(t, database); attempts != 1 {
		t.Fatalf("first pass: attempts=%d, want 1", attempts)
	}

	for pass := 2; pass <= 3; pass++ {
		st, err := ResolveForFiles(database, t.TempDir(), []string{"a.go"})
		if err != nil {
			t.Fatal(err)
		}
		attempts, status := refState(t, database)
		if attempts != pass {
			t.Fatalf("pass %d: attempts=%d", pass, attempts)
		}
		if pass < 3 {
			if st.Abandoned != 0 || status != "failed" {
				t.Fatalf("pass %d: st=%+v status=%s", pass, st, status)
			}
		} else if st.Abandoned != 1 || status != "abandoned" {
			t.Fatalf("pass %d at cap: st=%+v status=%s", pass, st, status)
		}
	}

	// Abandoned rows are not selected by the incremental queries either.
	st, err := ResolveForFiles(database, t.TempDir(), []string{"a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Failed != 0 || st.Retried != 0 || st.Abandoned != 0 {
		t.Fatalf("post-cap incremental pass must skip the abandoned ref, got %+v", st)
	}
}
