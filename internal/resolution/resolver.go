package resolution

import (
	"log"
	"path/filepath"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// Stats summarizes one ResolveAll pass.
type Stats struct {
	Resolved int
	Failed   int
	Retried  int
	// Abandoned is how many refs hit maxResolveAttempts this pass: their rows
	// were parked as 'abandoned' (kept for audit, no longer retried). A
	// subset of Failed — abandoning happens on a failed attempt.
	Abandoned int
}

// maxResolveAttempts is how many failed resolution attempts a ref may
// accumulate before it stops being retried: the failure that reaches the cap
// parks the row as 'abandoned' instead of 'failed' (kept for audit,
// excluded from every retry query). This bounds the per-pass work spent on
// refs whose targets can never exist in the index (e.g. standard-library and
// builtin calls). Var, not const, so tests can shrink it.
var maxResolveAttempts = 20

// ResolveAll turns pending (and retryable failed) unresolved_refs into edges.
// Cross-file graph edges are born here — not during extraction (step 2/3 split).
func ResolveAll(database *db.DB, workdir string) (Stats, error) {
	var st Stats

	pending, err := database.ListUnresolvedRefs("", "pending")
	if err != nil {
		return st, err
	}
	failed, err := database.ListUnresolvedRefs("", "failed")
	if err != nil {
		return st, err
	}
	// Retry failed refs when a candidate name now exists.
	var retry []db.UnresolvedRef
	for _, r := range failed {
		cands, err := CollectCandidates(database, r.ReferenceName)
		if err != nil || len(cands) == 0 {
			continue
		}
		retry = append(retry, r)
	}
	st.Retried = len(retry)

	batch := append(pending, retry...)
	w := newEdgeWriter(database)
	for _, r := range batch {
		plan, err := resolveOne(database, workdir, r)
		if err != nil {
			// A per-ref resolution error is a failure for THIS ref this pass:
			// count it and park the ref as failed (failed rows are retried by
			// later passes whenever a candidate name exists, so retry
			// semantics are preserved and stats no longer under-report). Each
			// failure bumps attempts; once maxResolveAttempts is reached the
			// row is parked as 'abandoned' and later passes stop selecting it.
			log.Printf("resolve ref %s: %v", r.ReferenceName, err)
			st.Failed++
			if markRefFailed(database, r, maxResolveAttempts) {
				st.Abandoned++
			}
			continue
		}
		if plan == nil {
			st.Failed++
			if markRefFailed(database, r, maxResolveAttempts) {
				st.Abandoned++
			}
			continue
		}
		w.add(*plan)
	}
	w.flush()
	st.Resolved += w.resolved
	for _, p := range w.failed {
		st.Failed++
		if markRefFailed(database, p.ref, maxResolveAttempts) {
			st.Abandoned++
		}
	}
	return st, nil
}

// resolveOne decides the resolution outcome for one ref without writing:
// it returns the plan to persist (edge upsert + pending-ref delete), nil
// when the ref has no resolvable match (the caller parks it as failed), or
// an error when the read-side lookups fail. Persistence itself is batched
// through edgeWriter so a pass does not commit per ref.
func resolveOne(database *db.DB, workdir string, r db.UnresolvedRef) (*edgePlan, error) {
	if r.FromNode == 0 || r.ReferenceName == "" {
		return nil, nil
	}
	kind := r.ReferenceKind
	if kind == "" {
		kind = db.EdgeCalls
	}
	preferCall := kind == db.EdgeCalls || kind == db.EdgeReferences || kind == "bridge"

	candidates, err := CollectCandidates(database, r.ReferenceName)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Never link a node to itself.
	filtered := candidates[:0]
	for _, c := range candidates {
		if c.ID != r.FromNode {
			filtered = append(filtered, c)
		}
	}
	candidates = filtered
	if len(candidates) == 0 {
		return nil, nil
	}

	lang := r.Language
	fromFile := r.FilePath

	// Import-closure preference
	if imp, ok := FilterByImports(database, workdir, fromFile, lang, candidates); ok {
		m := MatchName(imp, r.ReferenceName, fromFile, preferCall)
		if m.TargetID != 0 {
			return &edgePlan{ref: r, targetID: m.TargetID, kind: kind, provenance: ProvImport}, nil
		}
	}

	m := MatchName(candidates, r.ReferenceName, fromFile, preferCall)
	if m.TargetID != 0 && m.Provenance == ProvHeuristic && preferCall {
		// No file/directory proximity: reject to let unique-callable check handle it.
		m = MatchResult{}
	}
	if m.TargetID == 0 {
		// Unique global callable: accept as heuristic.
		if preferCall {
			var callables []db.Node
			for _, c := range candidates {
				if callTargetKinds[c.Kind] {
					callables = append(callables, c)
				}
			}
			if len(callables) == 1 {
				// Only accept a unique callable when supported by import closure,
				// same-directory, or direct-subdirectory proximity (the candidate
				// lives inside a subdirectory of the caller's directory).
				cdir := filepath.Dir(callables[0].File)
				fromDir := filepath.Dir(fromFile)
				if _, ok := FilterByImports(database, workdir, fromFile, lang, []db.Node{callables[0]}); ok {
					m = MatchResult{TargetID: callables[0].ID, Provenance: ProvImport}
				} else if cdir == fromDir {
					m = MatchResult{TargetID: callables[0].ID, Provenance: ProvHeuristic}
				} else if filepath.Dir(cdir) == fromDir {
					m = MatchResult{TargetID: callables[0].ID, Provenance: ProvHeuristic}
				}
			}
		}
	}
	if m.TargetID == 0 {
		return nil, nil
	}
	if m.Provenance == "" {
		m.Provenance = ProvHeuristic
	}
	return &edgePlan{ref: r, targetID: m.TargetID, kind: kind, provenance: m.Provenance}, nil
}

// resolveBatchSize is how many resolved refs are persisted per write
// transaction. Batching replaced two implicit autocommit transactions per
// ref (edge upsert + ref delete): a full-table ResolveAll over a 100k-ref
// backlog used to issue ~200k commits, each an fsync under the single-writer
// lock. 500 keeps each transaction (and its lock hold) short while
// amortizing the commit cost ~1000x. A var so tests can shrink it.
var resolveBatchSize = 500

// edgePlan is one resolved ref whose persistence is still pending: upsert
// the edge, then delete the unresolved_ref.
type edgePlan struct {
	ref        db.UnresolvedRef
	targetID   int64
	kind       string
	provenance string
}

// edgeWriter accumulates resolved refs and persists them in batched write
// transactions (db.ApplyResolvedRefs) instead of two autocommit writes per
// ref. Flush keeps the per-ref contract of the pre-batching path: when the
// batch transaction fails, every plan in it is retried individually to
// isolate the offending row, so one bad ref can neither fail the whole pass
// nor take good refs down with it. Persisted plans are counted in resolved;
// plans that still fail the per-ref retry are recorded in failed (the
// caller logs, counts them as failures and parks their refs).
type edgeWriter struct {
	database *db.DB
	batch    []edgePlan
	resolved int
	failed   []edgePlan
}

func newEdgeWriter(database *db.DB) *edgeWriter {
	return &edgeWriter{database: database, batch: make([]edgePlan, 0, resolveBatchSize)}
}

// add queues a plan, flushing a full batch first when the buffer is at cap.
func (w *edgeWriter) add(p edgePlan) {
	w.batch = append(w.batch, p)
	if len(w.batch) >= resolveBatchSize {
		w.flush()
	}
}

// applyOne persists a single plan with the pre-batching per-ref semantics:
// the edge is upserted, then the pending ref row is deleted.
func (w *edgeWriter) applyOne(p edgePlan) error {
	if _, err := w.database.UpsertEdge(&db.Edge{
		SourceID:   p.ref.FromNode,
		TargetID:   p.targetID,
		Kind:       p.kind,
		File:       p.ref.FilePath,
		Line:       p.ref.Line,
		Col:        p.ref.Col,
		Provenance: p.provenance,
	}); err != nil {
		return err
	}
	return w.database.DeleteUnresolvedRef(p.ref.ID)
}

// flush persists the buffered plans atomically: the whole batch upserts and
// deletes inside one transaction, so a crash mid-batch leaves the graph
// consistent (either both sides of every ref landed or none did). If the
// batch fails, each plan is retried individually so a single bad row (e.g.
// an edge violating a constraint) is isolated instead of poisoning the batch.
func (w *edgeWriter) flush() {
	if len(w.batch) == 0 {
		return
	}
	refs := make([]db.ResolvedRef, len(w.batch))
	for i, p := range w.batch {
		refs[i] = db.ResolvedRef{
			UnresolvedID: p.ref.ID,
			Edge: db.Edge{
				SourceID:   p.ref.FromNode,
				TargetID:   p.targetID,
				Kind:       p.kind,
				File:       p.ref.FilePath,
				Line:       p.ref.Line,
				Col:        p.ref.Col,
				Provenance: p.provenance,
			},
		}
	}
	if err := w.database.ApplyResolvedRefs(refs); err != nil {
		for _, p := range w.batch {
			if w.applyOne(p) == nil {
				w.resolved++
			} else {
				w.failed = append(w.failed, p)
			}
		}
	} else {
		w.resolved += len(w.batch)
	}
	w.batch = w.batch[:0]
}

// markRefFailed records one failed attempt for a ref (backfilling an empty
// stored name_tail from the reference name): the row is parked as 'failed'
// for later retry passes, or as 'abandoned' when the attempt reaches
// maxAttempts — abandoned rows stay in the table for audit but are no longer
// selected by the pending/failed retry queries. Reports whether this call
// abandoned the row.
func markRefFailed(database *db.DB, r db.UnresolvedRef, maxAttempts int) bool {
	tail := r.NameTail
	if tail == "" {
		tail = nameTail(r.ReferenceName)
	}
	abandoned, err := database.MarkUnresolvedFailed(r.ID, tail, maxAttempts)
	if err != nil {
		return false
	}
	return abandoned
}

// ResolveForFiles re-runs resolution focusing on refs from the given files
// (and retries failed tails that may now resolve after those files changed).
func ResolveForFiles(database *db.DB, workdir string, files []string) (Stats, error) {
	if len(files) == 0 {
		return ResolveAll(database, workdir)
	}
	want := map[string]bool{}
	for _, f := range files {
		want[f] = true
	}
	var st Stats

	// Load refs for changed files directly via SQL (avoid full-table scan + Go filter).
	pendingByFiles, err := database.ListUnresolvedRefsByFiles(files, "pending")
	if err != nil {
		return st, err
	}
	failedByFiles, err := database.ListUnresolvedRefsByFiles(files, "failed")
	if err != nil {
		return st, err
	}
	var batch []db.UnresolvedRef
	batch = append(batch, pendingByFiles...)
	batch = append(batch, failedByFiles...)

	// Also any pending/failed whose name might be defined in changed files.
	changedNames := map[string]bool{}
	for _, f := range files {
		// ForEach paginates through ALL matches: a file with more symbols
		// than getNodesByFileCap must not silently lose name candidates.
		err := database.ForEachNodeByFileLight(f, func(n db.Node) error {
			changedNames[n.Name] = true
			return nil
		})
		if err != nil {
			continue
		}
	}
	if len(changedNames) > 0 {
		// F1: push the name filter down to SQL — matching is exact equality on
		// reference_name / name_tail (the map lookup below used to run against
		// a full-table ListUnresolvedRefs("", status) scan in Go).
		names := make([]string, 0, len(changedNames))
		for n := range changedNames {
			names = append(names, n)
		}
		refs, err := database.ListUnresolvedRefsByNames(names, []string{"pending", "failed"})
		if err != nil {
			return st, err
		}
		for _, r := range refs {
			if want[r.FilePath] {
				continue // already queued via ByFiles
			}
			batch = append(batch, r)
			if r.Status == "failed" {
				st.Retried++
			}
		}
		// S2 (F1 regression): the SQL pushdown matches stored name_tail
		// exactly, but historical/anomalous rows may carry name_tail='' while
		// reference_name holds the full qualified name (e.g. "pkg.Foo") and
		// the changed symbol is the bare tail ("Foo"). The old full-table Go
		// scan matched those via nameTail(reference_name); re-apply that for
		// empty-tail rows only — the SQL branches above already cover every
		// non-empty tail row and every reference_name exact match.
		emptyTail, err := database.ListUnresolvedRefsEmptyTail([]string{"pending", "failed"})
		if err != nil {
			return st, err
		}
		for _, r := range emptyTail {
			if want[r.FilePath] {
				continue // already queued via ByFiles
			}
			if changedNames[r.ReferenceName] {
				continue // already returned by the reference_name IN branch
			}
			if changedNames[nameTail(r.ReferenceName)] {
				batch = append(batch, r)
				if r.Status == "failed" {
					st.Retried++
				}
			}
		}
	}
	// Dedupe by id
	seen := map[int64]bool{}
	w := newEdgeWriter(database)
	for _, r := range batch {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		plan, err := resolveOne(database, workdir, r)
		if err != nil {
			log.Printf("resolve ref %s: %v", r.ReferenceName, err)
			st.Failed++
			// Previously failed refs are marked too (not only pending ones):
			// every attempted-and-failed ref must age, or rows that keep
			// failing on incremental passes would never reach the cap.
			if markRefFailed(database, r, maxResolveAttempts) {
				st.Abandoned++
			}
			continue
		}
		if plan == nil {
			st.Failed++
			if markRefFailed(database, r, maxResolveAttempts) {
				st.Abandoned++
			}
			continue
		}
		w.add(*plan)
	}
	w.flush()
	st.Resolved += w.resolved
	for _, p := range w.failed {
		st.Failed++
		if markRefFailed(database, p.ref, maxResolveAttempts) {
			st.Abandoned++
		}
	}
	return st, nil
}
