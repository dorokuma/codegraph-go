package resolution

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/codegraph-go/internal/db"
	"modernc.org/sqlite"
)

var cancellationLookupID atomic.Uint64

// Register before opening the scratch DB, then put the function into a view
// over its node table. This cancels inside real SQL, without a production hook.
func registerCancelLookup(t *testing.T, lookup func(string)) string {
	t.Helper()
	name := fmt.Sprintf("cancel_lookup_%d", cancellationLookupID.Add(1))
	if err := sqlite.RegisterScalarFunction(name, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if value, ok := args[0].(string); ok {
			lookup(value)
		}
		return args[0], nil
	}); err != nil {
		t.Fatal(err)
	}
	return name
}

func installCancelLookup(t *testing.T, database *db.DB, function string) func() {
	t.Helper()
	conn, err := sql.Open("sqlite", database.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	for _, query := range []string{
		`ALTER TABLE nodes RENAME TO cancellation_nodes`,
		`CREATE VIEW nodes AS SELECT id, kind, name,
		file, line, end_line, ` + function + `(name) AS body, language, qualified_name, signature, docstring,
		start_column, end_column, visibility, is_exported, return_type FROM cancellation_nodes`,
	} {
		if _, err := conn.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		for _, query := range []string{`DROP VIEW nodes`, `ALTER TABLE cancellation_nodes RENAME TO nodes`} {
			if _, err := conn.Exec(query); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestResolutionCancellationPreservesPendingRefs(t *testing.T) {
	for _, mode := range []string{"all", "files", "empty-files"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			function := registerCancelLookup(t, func(name string) {
				if name == "target" && calls.Add(1) == 2 {
					cancel() // first ref is planned, second ref's lookup is interrupted
				}
			})
			database, caller, _, _ := newBatchTestDB(t, 3)
			restore := installCancelLookup(t, database, function)
			var st Stats
			var err error
			switch mode {
			case "all":
				st, err = ResolveAllContext(ctx, database, t.TempDir())
			case "files":
				st, err = ResolveForFilesContext(ctx, database, t.TempDir(), []string{"a.go"})
			case "empty-files":
				st, err = ResolveForFilesContext(ctx, database, t.TempDir(), nil)
			}
			if !errors.Is(err, context.Canceled) || st != (Stats{}) || calls.Load() < 2 {
				t.Fatalf("active resolution: calls=%d stats=%+v err=%v", calls.Load(), st, err)
			}
			restore()
			refs, err := database.ListUnresolvedRefs("", "")
			if err != nil || len(refs) != 3 {
				t.Fatalf("refs=%v err=%v", refs, err)
			}
			for _, r := range refs {
				if r.Status != "pending" {
					t.Fatalf("cancellation aged a reference: %+v", r)
				}
			}
			// A later pass must resolve even the plan buffered before cancellation.
			st, err = ResolveAll(database, t.TempDir())
			if err != nil || st.Resolved != 3 {
				t.Fatalf("retry: stats=%+v err=%v", st, err)
			}
			edges, err := database.GetOutgoingEdges(caller, []string{db.EdgeCalls})
			if err != nil || len(edges) != 3 {
				t.Fatalf("retry edges=%v err=%v", edges, err)
			}
		})
	}
}

func TestResolutionReadsHonorCancellation(t *testing.T) {
	database, _, _, _ := newBatchTestDB(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := database.ListUnresolvedRefsContext(ctx, "", "pending"); !errors.Is(err, context.Canceled) {
		t.Fatalf("reference scan ignored cancellation: %v", err)
	}
	if _, err := collectCandidatesContext(ctx, database, "target"); !errors.Is(err, context.Canceled) {
		t.Fatalf("candidate lookup ignored cancellation: %v", err)
	}
	if _, err := database.GetImportTargetNamesContext(ctx, "a.go"); !errors.Is(err, context.Canceled) {
		t.Fatalf("import lookup ignored cancellation: %v", err)
	}
}

// newBatchTestDB opens a scratch index DB with two real nodes (caller in
// a.go, target in b.go) and n pending unresolved_refs anchored on caller,
// one per distinct call-site line so each resolves to its own edge row.
func newBatchTestDB(t *testing.T, n int) (*db.DB, int64, int64, []db.UnresolvedRef) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "idx"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	caller, err := database.UpsertNode(&db.Node{Kind: "function", Name: "caller", File: "a.go", Line: 1, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := database.UpsertNode(&db.Node{Kind: "function", Name: "target", File: "b.go", Line: 1, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	var refs []db.UnresolvedRef
	for i := 0; i < n; i++ {
		id, err := database.InsertUnresolvedRef(&db.UnresolvedRef{
			FromNode:      caller,
			ReferenceName: "target",
			ReferenceKind: db.EdgeCalls,
			Line:          10 + i,
			FilePath:      "a.go",
			Language:      "go",
			Status:        "pending",
		})
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, db.UnresolvedRef{
			ID:            id,
			FromNode:      caller,
			ReferenceName: "target",
			ReferenceKind: db.EdgeCalls,
			Line:          10 + i,
			FilePath:      "a.go",
			Language:      "go",
			Status:        "pending",
		})
	}
	return database, caller, target, refs
}

func planFor(r db.UnresolvedRef, target int64) edgePlan {
	return edgePlan{ref: r, targetID: target, kind: db.EdgeCalls, provenance: ProvHeuristic}
}

// TestEdgeWriterBatchesWrites covers the happy path: plans larger than
// resolveBatchSize are persisted across multiple whole-batch transactions,
// with the same final state the per-ref path produces — every edge upserted
// and every pending unresolved_ref deleted.
func TestEdgeWriterBatchesWrites(t *testing.T) {
	database, caller, target, refs := newBatchTestDB(t, 3)

	oldSize := resolveBatchSize
	resolveBatchSize = 2
	t.Cleanup(func() { resolveBatchSize = oldSize })

	w := newEdgeWriter(database)
	for _, r := range refs {
		w.add(planFor(r, target)) // flushes at cap: batch of 2, then batch of 1
	}
	w.flush()
	if w.resolved != 3 || len(w.failed) != 0 {
		t.Fatalf("resolved=%d failed=%d, want 3/0", w.resolved, len(w.failed))
	}
	left, err := database.ListUnresolvedRefs("", "")
	if err != nil || len(left) != 0 {
		t.Fatalf("pending refs left = %v (err %v), want none", left, err)
	}
	edges, err := database.GetOutgoingEdges(caller, []string{db.EdgeCalls})
	if err != nil || len(edges) != 3 {
		t.Fatalf("edges = %d (err %v), want 3 (one per call site)", len(edges), err)
	}
}

// TestEdgeWriterFallsBackPerRefOnBatchFailure injects one plan whose edge
// violates the edges.source_id foreign key. The whole batch transaction must
// roll back, and the flush must retry each plan individually so the good
// plans still land while only the bad one is reported as failed — the
// caller then parks exactly that ref.
func TestEdgeWriterFallsBackPerRefOnBatchFailure(t *testing.T) {
	database, caller, target, refs := newBatchTestDB(t, 3)

	// Poison the middle plan: its edge source names a node that does not
	// exist, so the upsert hits the edges FK inside the batch transaction.
	poisoned := refs[1]
	poisoned.FromNode = 999_999_999
	plans := []edgePlan{
		planFor(refs[0], target),
		planFor(poisoned, target),
		planFor(refs[2], target),
	}

	w := newEdgeWriter(database)
	for _, p := range plans {
		w.add(p)
	}
	w.flush()
	if w.resolved != 2 {
		t.Fatalf("resolved=%d, want 2 (good plans isolated from the bad row)", w.resolved)
	}
	if len(w.failed) != 1 || w.failed[0].ref.ID != refs[1].ID {
		t.Fatalf("failed = %+v, want exactly the poisoned ref %d", w.failed, refs[1].ID)
	}

	// Good plans landed: their edges exist and their refs are gone.
	edges, err := database.GetOutgoingEdges(caller, []string{db.EdgeCalls})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Fatalf("edges = %d, want 2", len(edges))
	}
	left, err := database.ListUnresolvedRefs("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != refs[1].ID {
		t.Fatalf("remaining refs = %+v, want only the poisoned ref %d", left, refs[1].ID)
	}
}

// TestResolveScopeCachesCandidates covers the per-pass candidate cache: refs
// sharing a reference_name reuse the first lookup (counted via a scalar
// function on the name column), and a cancellation fired inside that SQL
// still lands on the NEXT ref — never on a replayed cache value.
func TestResolveScopeCachesCandidates(t *testing.T) {
	var calls atomic.Int32
	function := registerCancelLookup(t, func(name string) { calls.Add(1) })
	database, _, _, _ := newBatchTestDB(t, 3)
	restore := installCancelLookup(t, database, function)
	defer restore()

	st, err := ResolveAll(database, t.TempDir())
	if err != nil || st.Resolved != 3 {
		t.Fatalf("stats=%+v err=%v, want 3 resolved", st, err)
	}
	// Three refs on the same name: the first fills the cache (one full
	// collectCandidatesContext query); the other two each run the cheap
	// NodeNameExistsContext probe instead of a second full query. Exactly 3
	// name-touching evaluations — any extra would mean a cache miss reran the
	// full scan, any fewer a probe was skipped.
	if got := calls.Load(); got != 3 {
		t.Fatalf("name lookups=%d, want exactly 3 (one full + two probes)", got)
	}
}

// TestResolveOneSelfRefFilterKeepsCandidateCache is the regression for the
// in-place filter on the cached candidate slice: resolveOne used to write
// the self-reference filter back into s.cand's shared backing array via
// candidates[:0], so a first ref whose FromNode matched a candidate would
// permanently erase (and duplicate) entries for every later ref on the
// same name. Two refs share ReferenceName "target": the first one IS the
// target node (self-ref, filtered out), the second is a real caller that
// must still resolve to it — with the cache holding exactly one untampered
// candidate afterwards.
func TestResolveOneSelfRefFilterKeepsCandidateCache(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "idx"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	target, err := database.UpsertNode(&db.Node{Kind: "function", Name: "target", File: "a.go", Line: 1, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	caller, err := database.UpsertNode(&db.Node{Kind: "function", Name: "caller", File: "a.go", Line: 10, Language: "go"})
	if err != nil {
		t.Fatal(err)
	}

	s := newResolveScope(database, t.TempDir())
	ctx := context.Background()
	selfRef := db.UnresolvedRef{
		FromNode:      target,
		ReferenceName: "target",
		ReferenceKind: db.EdgeCalls,
		Line:          2,
		FilePath:      "a.go",
		Language:      "go",
		Status:        "pending",
	}
	// First ref is a self-reference: after filtering, no plan is produced,
	// but the cached candidate list must stay intact.
	plan, err := s.resolveOne(ctx, selfRef)
	if err != nil || plan != nil {
		t.Fatalf("self-ref resolveOne: plan=%+v err=%v, want nil plan", plan, err)
	}

	cached := s.cand["target"]
	if len(cached) != 1 || cached[0].ID != target {
		t.Fatalf("candidate cache corrupted: %+v, want exactly [target id=%d]", cached, target)
	}

	// The second ref is a different node calling the same name: it must
	// still see the target candidate and resolve to it.
	callRef := db.UnresolvedRef{
		FromNode:      caller,
		ReferenceName: "target",
		ReferenceKind: db.EdgeCalls,
		Line:          11,
		FilePath:      "a.go",
		Language:      "go",
		Status:        "pending",
	}
	plan, err = s.resolveOne(ctx, callRef)
	if err != nil {
		t.Fatalf("second ref resolveOne: %v", err)
	}
	if plan == nil || plan.targetID != target {
		t.Fatalf("second ref plan=%+v, want targetID=%d (cached candidate list was not corrupted)", plan, target)
	}

	// Cache still holds the original single candidate, unmodified.
	cached = s.cand["target"]
	if len(cached) != 1 || cached[0].ID != target {
		t.Fatalf("candidate cache after second ref: %+v, want exactly [target id=%d]", cached, target)
	}
}

// TestFailureWriterBatchesMarks verifies the queued failed-ref markings land
// with the same semantics as the per-ref path (attempts+1, failed→abandoned
// at cap) while committing once per flush.
func TestFailureWriterBatchesMarks(t *testing.T) {
	database, _, _, _ := newBatchTestDB(t, 2)

	// Doom both refs: no node named 'ghost' exists, so both refs fail.
	refs, err := database.ListUnresolvedRefs("", "pending")
	if err != nil || len(refs) != 2 {
		t.Fatalf("refs=%v err=%v", refs, err)
	}
	for i := range refs {
		refs[i].ReferenceName = "ghost" + fmt.Sprint(i)
	}

	fw := newFailureWriterContext(context.Background(), database)
	for _, r := range refs {
		if fw.markFailed(r, 2) {
			t.Fatal("queueing must never report abandoned")
		}
	}
	if n := fw.flush(2); n != 0 {
		t.Fatalf("flush abandoned=%d, want 0 (cap 2, attempt 1)", n)
	}
	for _, r := range refs {
		attempts, err := database.UnresolvedRefAttempts(r.ID)
		if err != nil || attempts != 1 {
			t.Fatalf("id %d attempts=%d err=%v, want 1", r.ID, attempts, err)
		}
	}

	// Second batch reaches the cap.
	fw2 := newFailureWriterContext(context.Background(), database)
	for _, r := range refs {
		fw2.markFailed(r, 2)
	}
	if n := fw2.flush(2); n != 2 {
		t.Fatalf("flush 2 abandoned=%d, want 2", n)
	}
	if n, _ := database.CountUnresolvedRefs("abandoned"); n != 2 {
		t.Fatalf("abandoned count=%d, want 2", n)
	}

	// A cancelled context leaves queued refs untouched.
	fw3ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fw3 := newFailureWriterContext(fw3ctx, database)
	fw3.markFailed(refs[0], 5)
	if n := fw3.flush(5); n != 0 {
		t.Fatalf("cancelled flush must abandon nothing, got %d", n)
	}
}

// TestResolveAllContextCachesImportClosure verifies same-file refs reuse
// the import-closure load: one file's import specs are queried once per
// scope no matter how many refs that file carries (the old path ran the
// imports-edge query + os.Stat resolution per ref).
func TestResolveAllContextCachesImportClosure(t *testing.T) {
	database, caller, _, _ := newBatchTestDB(t, 5)

	// Record one imports edge on the caller's file so the closure load
	// actually queries and resolves a spec each pass.
	mod, err := database.UpsertNode(&db.Node{Kind: "module", Name: "bmod", File: "bmod", Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpsertEdge(&db.Edge{SourceID: caller, TargetID: mod, Kind: "imports", File: "a.go", Line: 1}); err != nil {
		t.Fatal(err)
	}

	// Same (file, lang) key → the second lookup replays the cached closure
	// object (pointer identity), proving no second spec query/Stat round.
	s := newResolveScope(database, t.TempDir())
	c1, err := s.importClosure(context.Background(), "a.go", "go")
	if err != nil || c1 == nil || len(c1.specs) == 0 {
		t.Fatalf("first closure: c=%+v err=%v, want specs present", c1, err)
	}
	c2, err := s.importClosure(context.Background(), "a.go", "go")
	if err != nil || c2 != c1 {
		t.Fatalf("second lookup must replay the cached closure (c2=%p c1=%p err=%v)", c2, c1, err)
	}
	// A different file resolves its own closure — no cross-file bleed.
	c3, err := s.importClosure(context.Background(), "other.go", "go")
	if err != nil || c3 == c1 {
		t.Fatalf("different file must not share the closure (c3=%p c1=%p err=%v)", c3, c1, err)
	}

	// End-to-end: 5 same-file refs resolve through ONE pass, each landing
	// its own call-site edge on target.
	st, err := ResolveAll(database, t.TempDir())
	if err != nil || st.Resolved != 5 {
		t.Fatalf("stats=%+v err=%v, want 5 resolved", st, err)
	}
	edges, err := database.GetOutgoingEdges(caller, []string{db.EdgeCalls})
	if err != nil || len(edges) != 5 {
		t.Fatalf("edges=%v err=%v, want 5 call-site edges", edges, err)
	}
}

// TestCancelledFlushDropsQueuedMarks is the S1 regression: a ref that was
// already queued via markFailed must NOT be counted as a persisted failure
// (or aged) when cancellation drops the whole failure queue. The pass
// leaves the ref's attempts/status untouched and Stats.Failed/Abandoned
// report the rows that were actually committed — zero, not the attempts.
func TestCancelledFlushDropsQueuedMarks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	// Cancel inside the candidate lookup for "target" — the SECOND ref's
	// resolution. The first ref ("ghost") has no candidate and is already
	// queued through markFailed (st.Failed++ + an entry in fw.marks) before
	// the second ref's lookup fires the cancel.
	function := registerCancelLookup(t, func(name string) {
		if name == "target" {
			calls.Add(1)
			cancel()
		}
	})
	database, caller, _, _ := newBatchTestDB(t, 0)
	restore := installCancelLookup(t, database, function)
	defer restore()

	// Ref 1 fails (no "ghost" node → plan nil → markFailed queued); ref 2
	// names "target" whose candidate lookup fires the cancel. The ghost ref
	// must come first in the pending scan so it is queued before the cancel.
	var refs []db.UnresolvedRef
	for i, name := range []string{"ghost", "target"} {
		id, err := database.InsertUnresolvedRef(&db.UnresolvedRef{
			FromNode: caller, ReferenceName: name, ReferenceKind: db.EdgeCalls,
			Line: 20 + i, FilePath: "a.go", Language: "go", Status: "pending",
		})
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, db.UnresolvedRef{ID: id, ReferenceName: name})
	}

	st, err := ResolveAllContext(ctx, database, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got stats=%+v err=%v", st, err)
	}

	// Stats must match the persisted state: the cancelled flush dropped the
	// queue, so nothing was persisted as failed/abandoned.
	if st.Failed != 0 || st.Abandoned != 0 {
		t.Fatalf("stats after cancelled flush = %+v, want Failed=0 Abandoned=0 "+
			"(queued marks were dropped, not persisted)", st)
	}

	// Every pending ref is untouched: attempts unchanged, status still pending
	// (cancellation ages nothing — the whole pass is retried on the next run).
	for _, r := range refs {
		attempts, aerr := database.UnresolvedRefAttempts(r.ID)
		if aerr != nil || attempts != 0 {
			t.Fatalf("ref %d attempts=%d err=%v, want 0 (cancel must not age)", r.ID, attempts, aerr)
		}
	}
	left, err := database.ListUnresolvedRefs("", "pending")
	if err != nil || len(left) != len(refs) {
		t.Fatalf("pending refs=%d err=%v, want %d still pending", len(left), err, len(refs))
	}
	if calls.Load() < 1 {
		t.Fatalf("cancel hook never fired (calls=%d): the cancel did not happen "+
			"inside the target lookup as intended", calls.Load())
	}
}

// TestResolveAllRepeatedNameRefsIsFast is the scale regression for the
// per-pass caches: a backlog of refs on one file and one name must resolve
// quickly — the un-cached path re-ran the candidate query and the import
// closure load per ref (O(refs) index scans + os.Stat bursts).
func TestResolveAllRepeatedNameRefsIsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("scale check")
	}
	database, caller, target, _ := newBatchTestDB(t, 400)
	_ = caller
	start := time.Now()
	st, err := ResolveAll(database, t.TempDir())
	elapsed := time.Since(start)
	if err != nil || st.Resolved != 400 {
		t.Fatalf("stats=%+v err=%v, want 400 resolved", st, err)
	}
	_ = target
	if elapsed > 30*time.Second {
		t.Fatalf("400 same-name refs took %v — candidate/import caching is not effective", elapsed)
	}
}
