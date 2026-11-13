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
