package resolution

import (
	"context"
	"log"
	"path/filepath"
	"strings"

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
	return ResolveAllContext(context.Background(), database, workdir)
}

// ResolveAllContext stops between references and cancels read-side SQL on shutdown.
// Unflushed plans remain pending in the DB and are retried on the next pass.
func ResolveAllContext(ctx context.Context, database *db.DB, workdir string) (Stats, error) {
	var st Stats
	if err := ctx.Err(); err != nil {
		return st, err
	}

	// Per-pass cache scope: failed-retry lookups, import-closure files and
	// candidate queries are stable within one pass (the pass only writes
	// edges + unresolved_refs, never nodes), so same-file/same-name repeats
	// reuse the first lookup instead of re-querying per ref. The scope ends
	// with this call — no state leaks between passes (fresh files indexed by
	// the NEXT pass are re-resolved there, exactly as before).
	s := newResolveScope(database, workdir)

	pending, err := database.ListUnresolvedRefsContext(ctx, "", "pending")
	if err != nil {
		return st, err
	}
	failed, err := database.ListUnresolvedRefsContext(ctx, "", "failed")
	if err != nil {
		return st, err
	}
	// Retry failed refs when a candidate name now exists. The candidates
	// lookup is cached per reference_name within this pass: hundreds of
	// thousands of failed rows share a small name space, and the previous
	// per-row query ran once here and again inside resolveOneContext.
	var retry []db.UnresolvedRef
	for _, r := range failed {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		hit, err := s.hasCandidates(ctx, r.ReferenceName)
		if cerr := ctx.Err(); cerr != nil {
			return st, cerr
		}
		if err != nil || !hit {
			continue
		}
		retry = append(retry, r)
	}
	st.Retried = len(retry)

	batch := append(pending, retry...)
	w := newEdgeWriterContext(ctx, database)
	fw := newFailureWriterContext(ctx, database)
	for _, r := range batch {
		if err := ctx.Err(); err != nil {
			st.Resolved += w.resolved
			st.ReportDroppedFailureMarks(fw)
			return st, err
		}
		plan, err := s.resolveOne(ctx, r)
		// Cancellation is not a failed reference: do not age/abandon it.
		if cerr := ctx.Err(); cerr != nil {
			st.Resolved += w.resolved
			st.ReportDroppedFailureMarks(fw)
			return st, cerr
		}
		if err != nil {
			// A per-ref resolution error is a failure for THIS ref this pass:
			// count it and park the ref as failed (failed rows are retried by
			// later passes whenever a candidate name exists, so retry
			// semantics are preserved and stats no longer under-report). Each
			// failure bumps attempts; once maxResolveAttempts is reached the
			// row is parked as 'abandoned' and later passes stop selecting it.
			log.Printf("resolve ref %s: %v", r.ReferenceName, err)
			st.Failed++
			if fw.markFailed(r, maxResolveAttempts) {
				st.Abandoned++
			}
			continue
		}
		if plan == nil {
			st.Failed++
			if fw.markFailed(r, maxResolveAttempts) {
				st.Abandoned++
			}
			continue
		}
		w.add(*plan)
	}
	if err := ctx.Err(); err != nil {
		// Cancellation drops every still-queued mark: ReportedState keeps
		// Failed/Abandoned consistent with rows that were actually persisted.
		st.Resolved += w.resolved
		st.ReportDroppedFailureMarks(fw)
		return st, err
	}
	w.flush()
	st.Resolved += w.resolved
	for _, p := range w.failed {
		if err := ctx.Err(); err != nil {
			st.ReportDroppedFailureMarks(fw)
			return st, err
		}
		st.Failed++
		if fw.markFailed(p.ref, maxResolveAttempts) {
			st.Abandoned++
		}
	}
	// Persist the queued failed-ref markings in one commit; on batch failure
	// each ref falls back to the per-ref update so no failure is lost. A
	// cancelled flush persists nothing, so re-base the counters on what was
	// actually committed before returning the (non-nil) ctx error.
	st.Abandoned += fw.flush(maxResolveAttempts)
	st.ReportDroppedFailureMarks(fw)
	return st, ctx.Err()
}

// resolveScope carries the per-pass caches shared by every resolveOne call
// in one resolution pass. All three caches only memoize READ-SIDE lookups;
// the pass itself never inserts or updates nodes, so a cached candidate list
// or import-closure file set stays valid for the whole pass and is dropped
// with the scope afterwards.
type resolveScope struct {
	database *db.DB
	workdir  string
	// cand caches collectCandidatesContext results per reference name.
	cand map[string][]db.Node
	// hasCand caches the failed-retry existence check per reference name:
	// hundreds of thousands of failed rows share a small name space, and a
	// name that resolved to nothing moments ago still resolves to nothing.
	hasCand map[string]bool
	// imported caches each source file's import closure: the spec list on its
	// import edges plus the file set those specs resolve to on disk. Both
	// are candidate-independent, so the expensive query + os.Stat work runs
	// once per file per pass; the specMatchesFile seeding below stays
	// per-call exactly as before.
	imported map[importedKey]*importClosure
}

// importedKey identifies one file's import-closure lookup: the two edge-file
// keys tried by the import query plus the language the path resolver needs.
type importedKey struct {
	storeFrom string
	fromFile  string
	lang      string
}

func newResolveScope(database *db.DB, workdir string) *resolveScope {
	return &resolveScope{
		database: database,
		workdir:  workdir,
		cand:     map[string][]db.Node{},
		hasCand:  map[string]bool{},
		imported: map[importedKey]*importClosure{},
	}
}

// candidates returns the candidate node set for a reference name. The first
// call per name runs the full collectCandidatesContext query and stores the
// result; later calls for the same name replay it. Every call — hit or miss
// — still executes one real (cheap, body-free) existence query so the
// un-cached path's per-ref SQL cancellation surface is preserved exactly:
// a cancellation fired inside SQL still lands on the NEXT ref, never on a
// value replayed out of the map.
func (s *resolveScope) candidates(ctx context.Context, refName string) ([]db.Node, error) {
	refName = strings.TrimSpace(refName)
	if v, ok := s.cand[refName]; ok {
		if _, err := s.database.NodeNameExistsContext(ctx, refName); err != nil {
			return nil, err
		}
		return v, nil
	}
	nodes, err := collectCandidatesContext(ctx, s.database, refName)
	if err != nil {
		// Errors are NOT cached: each ref keeps the old per-row retry
		// behavior, and ctx cancellation must re-fire for the next caller.
		return nil, err
	}
	s.cand[refName] = nodes
	return nodes, nil
}

// hasCandidates runs the failed-retry existence check for a reference
// name, with the same hit-path probe query as candidates(): every ref
// touches SQL once so cancellation inside SQL lands on the NEXT ref.
func (s *resolveScope) hasCandidates(ctx context.Context, refName string) (bool, error) {
	refName = strings.TrimSpace(refName)
	if v, ok := s.hasCand[refName]; ok {
		if _, err := s.database.NodeNameExistsContext(ctx, refName); err != nil {
			return false, err
		}
		return v, nil
	}
	hit, err := hasCandidateContext(ctx, s.database, refName)
	if err != nil {
		return false, err
	}
	s.hasCand[refName] = hit
	return hit, nil
}

// importClosure returns the cached import closure for one source file,
// memoized per (storage key, raw path, lang). Load errors are not cached:
// the old path retried the query per ref, so each ref keeps that behavior,
// and a cancellation during the load is surfaced instead of stored.
func (s *resolveScope) importClosure(ctx context.Context, fromFile, lang string) (*importClosure, error) {
	k := importedKey{storeFrom: db.StoragePath(s.workdir, fromFile), fromFile: fromFile, lang: lang}
	if v, ok := s.imported[k]; ok {
		return v, nil
	}
	c, err := loadImportClosureContext(ctx, s.database, s.workdir, fromFile, lang)
	if err != nil {
		return nil, err
	}
	s.imported[k] = c
	return c, nil
}

// resolveOne decides the resolution outcome for one ref without writing:
// it returns the plan to persist (edge upsert + pending-ref delete), nil
// when the ref has no resolvable match (the caller parks it as failed), or
// an error when the read-side lookups fail. Persistence itself is batched
// through edgeWriter so a pass does not commit per ref.
func (s *resolveScope) resolveOne(ctx context.Context, r db.UnresolvedRef) (*edgePlan, error) {
	if r.FromNode == 0 || r.ReferenceName == "" {
		return nil, nil
	}
	kind := r.ReferenceKind
	if kind == "" {
		kind = db.EdgeCalls
	}
	preferCall := kind == db.EdgeCalls || kind == db.EdgeReferences || kind == "bridge"

	candidates, err := s.candidates(ctx, r.ReferenceName)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Never link a node to itself. The filter writes into a fresh slice:
	// candidates() returns the scope's cached array, so filtering in place
	// would corrupt s.cand[refName] for every later ref on this name.
	filtered := make([]db.Node, 0, len(candidates))
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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

	// Import-closure preference: specs and resolved files are per-source-file
	// (cached in the scope); the specMatchesFile seeding and candidate filter
	// still run per call against this ref's own candidate set.
	closure, cerr := s.importClosure(ctx, fromFile, lang)
	if cerr != nil {
		// Same contract as filterByImportsContext: a failed lookup keeps the
		// unfiltered candidates (ctx.Err() is re-checked below anyway).
		closure = &importClosure{}
	}
	if imp, ok := filterByImportedFiles(ctx, s.workdir, closure, candidates); ok {
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
				if err := ctx.Err(); err != nil {
					return nil, err
				}
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
				if _, ok := filterByImportedFiles(ctx, s.workdir, closure, []db.Node{callables[0]}); ok {
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
	ctx      context.Context
	database *db.DB
	batch    []edgePlan
	resolved int
	failed   []edgePlan
}

func newEdgeWriter(database *db.DB) *edgeWriter {
	return newEdgeWriterContext(context.Background(), database)
}

func newEdgeWriterContext(ctx context.Context, database *db.DB) *edgeWriter {
	return &edgeWriter{ctx: ctx, database: database, batch: make([]edgePlan, 0, resolveBatchSize)}
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
	if len(w.batch) == 0 || w.ctx.Err() != nil {
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
	// Do not initiate another transaction once cancellation is observed.
	// ApplyResolvedRefs uses a non-context transaction: once started, it
	// completes atomically despite cancellation. Stop new fallback writes too.
	if w.ctx.Err() != nil {
		return
	}
	if err := w.database.ApplyResolvedRefs(refs); err != nil {
		for _, p := range w.batch {
			if w.ctx.Err() != nil {
				return
			}
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

// failureWriter accumulates failed-ref markings and persists them in ONE
// batched transaction (db.ApplyUnresolvedFailed) at the end of a pass —
// no intermediate flushes because a crash mid-pass still leaves consistent
// rows (every marking is an independent attempts+1 update; losing the tail
// of a cancelled/failed pass only means those refs were never marked
// failed, identical to the pre-batching ordering where the per-ref commits
// for those refs had not happened yet either).
type failureWriter struct {
	ctx      context.Context
	database *db.DB
	marks    []failedMark
	// queuedAbandoned counts how many queued marks would already reach the
	// abandon cap, computed against the DB's CURRENT attempts value at queue
	// time. These are still only attempts: the DB row is unchanged until
	// flush commits, so Stats must report them as abandoned only once the
	// flush actually lands (see flush) — not when they are queued.
	queuedAbandoned int
	// persistedAbandoned is how many queuedAbandoned rows flush actually
	// committed as 'abandoned'. When a cancelled flush persists nothing this
	// stays 0, which is what lets Stats keep its abandoned count honest.
	persistedAbandoned int
}

// failedMark is one queued failed-attempt update: the ref and its stored
// name tail (backfilled from the reference name, as markRefFailed does).
type failedMark struct {
	ref  db.UnresolvedRef
	tail string
}

func newFailureWriterContext(ctx context.Context, database *db.DB) *failureWriter {
	return &failureWriter{ctx: ctx, database: database}
}

// markFailed queues one failed attempt for a ref (same name_tail backfill
// as markRefFailed) and always reports not-abandoned: queueing performs no
// write, so nothing can have been abandoned yet — the abandoned count is
// resolved when flush commits the batch and returned to the caller there.
// It still predicts (queuedAbandoned) which queued rows would cross the cap
// so a cancelled flush can keep Stats.Abandoned consistent with the rows
// that were actually persisted, instead of the queue length.
func (fw *failureWriter) markFailed(r db.UnresolvedRef, maxAttempts int) bool {
	tail := r.NameTail
	if tail == "" {
		tail = nameTail(r.ReferenceName)
	}
	// Predict abandonment from the row's CURRENT attempts (pre-write). The
	// row keeps its old attempts value until flush; if attempts+1 already
	// reaches the cap, flush would park it. Tracking this lets the caller
	// keep Stats consistent when cancellation drops the whole queue.
	if attempts, err := fw.database.UnresolvedRefAttempts(r.ID); err == nil && attempts+1 >= maxAttempts {
		fw.queuedAbandoned++
	}
	fw.marks = append(fw.marks, failedMark{ref: r, tail: tail})
	return false
}

// flush persists every queued marking in one transaction and returns how
// many refs it parked as abandoned. On batch failure it retries each ref
// through the old per-ref path (MarkUnresolvedFailed) so one bad row cannot
// lose the rest, matching edgeWriter's isolation contract. Cancellation is
// checked once up front (like edgeWriter.flush): a cancelled pass leaves
// queued refs unmarked so they are retried, never aged — in that case the
// whole queue is dropped, persistedAbandoned stays 0, and the caller must
// report the dropped marks so Stats stays consistent with the DB.
func (fw *failureWriter) flush(maxAttempts int) int {
	if len(fw.marks) == 0 || fw.ctx.Err() != nil {
		return 0
	}
	refs := make([]db.UnresolvedFailedRef, len(fw.marks))
	for i, m := range fw.marks {
		refs[i] = db.UnresolvedFailedRef{ID: m.ref.ID, NameTail: m.tail}
	}
	if fw.ctx.Err() != nil {
		return 0
	}
	abandoned, err := fw.database.ApplyUnresolvedFailed(refs, maxAttempts)
	if err != nil {
		n := 0
		for _, m := range fw.marks {
			if fw.ctx.Err() != nil {
				fw.persistedAbandoned += n
				return n
			}
			if markRefFailed(fw.database, m.ref, maxAttempts) {
				n++
			}
		}
		fw.persistedAbandoned += n
		return n
	}
	n := 0
	for _, a := range abandoned {
		if a {
			n++
		}
	}
	fw.persistedAbandoned += n
	return n
}

// droppedMarks is how many queued failure markings were never persisted.
// A mark counts as dropped while it sits in fw.marks (not yet flushed) or
// when the flush that owned it was cancelled. edgeWriter-buffered plans that
// surface as w.failed are counted by the caller before queueing them here,
// so this only needs to cover marks that reached markFailed.
func (fw *failureWriter) droppedMarks() int {
	// marks that were never committed. A successful flush consumed the whole
	// queue; a cancelled/aborted one leaves them. queuedAbandoned that were
	// never persisted count as dropped-abandoned too.
	if fw.ctx.Err() == nil {
		return 0 // nothing dropped unless cancellation aborted the flush
	}
	return len(fw.marks)
}

// ReportDroppedFailureMarks re-bases Failed/Abandoned on what the failure
// writer actually persisted after a cancellation. markFailed increments
// Stats.Failed and (predictively) Stats.Abandoned for every queued row, but
// a cancelled flush persists none of them — so the cancelled pass must
// subtract the still-queued attempts (and only the abandoned rows that
// actually committed). On a fully-committed flush there is nothing to drop.
func (st *Stats) ReportDroppedFailureMarks(fw *failureWriter) {
	dropped := fw.droppedMarks()
	st.Failed -= dropped
	st.Abandoned -= fw.queuedAbandoned - fw.persistedAbandoned
	if st.Failed < 0 {
		st.Failed = 0
	}
	if st.Abandoned < 0 {
		st.Abandoned = 0
	}
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
	return ResolveForFilesContext(context.Background(), database, workdir, files)
}

// ResolveForFilesContext is the cancellable incremental-resolution path.
func ResolveForFilesContext(ctx context.Context, database *db.DB, workdir string, files []string) (Stats, error) {
	if err := ctx.Err(); err != nil {
		return Stats{}, err
	}
	if len(files) == 0 {
		return ResolveAllContext(ctx, database, workdir)
	}
	want := map[string]bool{}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return Stats{}, err
		}
		want[f] = true
	}
	var st Stats

	// Load refs for changed files directly via SQL (avoid full-table scan + Go filter).
	pendingByFiles, err := database.ListUnresolvedRefsByFilesContext(ctx, files, "pending")
	if err != nil {
		return st, err
	}
	failedByFiles, err := database.ListUnresolvedRefsByFilesContext(ctx, files, "failed")
	if err != nil {
		return st, err
	}
	s := newResolveScope(database, workdir)

	var batch []db.UnresolvedRef
	batch = append(batch, pendingByFiles...)
	batch = append(batch, failedByFiles...)

	// Also any pending/failed whose name might be defined in changed files.
	changedNames := map[string]bool{}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		// ForEach paginates through ALL matches: a file with more symbols
		// than getNodesByFileCap must not silently lose name candidates.
		err := database.ForEachNodeByFileLightContext(ctx, f, func(n db.Node) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			changedNames[n.Name] = true
			return nil
		})
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return st, cerr
			}
			continue
		}
	}
	if len(changedNames) > 0 {
		// F1: push the name filter down to SQL — matching is exact equality on
		// reference_name / name_tail (the map lookup below used to run against
		// a full-table ListUnresolvedRefs("", status) scan in Go).
		names := make([]string, 0, len(changedNames))
		for n := range changedNames {
			if err := ctx.Err(); err != nil {
				return st, err
			}
			names = append(names, n)
		}
		refs, err := database.ListUnresolvedRefsByNamesContext(ctx, names, []string{"pending", "failed"})
		if err != nil {
			return st, err
		}
		for _, r := range refs {
			if err := ctx.Err(); err != nil {
				return st, err
			}
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
		emptyTail, err := database.ListUnresolvedRefsEmptyTailContext(ctx, []string{"pending", "failed"})
		if err != nil {
			return st, err
		}
		for _, r := range emptyTail {
			if err := ctx.Err(); err != nil {
				return st, err
			}
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
	w := newEdgeWriterContext(ctx, database)
	fw := newFailureWriterContext(ctx, database)
	for _, r := range batch {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		if err := ctx.Err(); err != nil {
			st.Resolved += w.resolved
			st.ReportDroppedFailureMarks(fw)
			return st, err
		}
		plan, err := s.resolveOne(ctx, r)
		// Cancellation is not a failed reference: do not age/abandon it.
		if cerr := ctx.Err(); cerr != nil {
			st.Resolved += w.resolved
			st.ReportDroppedFailureMarks(fw)
			return st, cerr
		}
		if err != nil {
			log.Printf("resolve ref %s: %v", r.ReferenceName, err)
			st.Failed++
			// Previously failed refs are marked too (not only pending ones):
			// every attempted-and-failed ref must age, or rows that keep
			// failing on incremental passes would never reach the cap.
			if fw.markFailed(r, maxResolveAttempts) {
				st.Abandoned++
			}
			continue
		}
		if plan == nil {
			st.Failed++
			if fw.markFailed(r, maxResolveAttempts) {
				st.Abandoned++
			}
			continue
		}
		w.add(*plan)
	}
	if err := ctx.Err(); err != nil {
		st.Resolved += w.resolved
		st.ReportDroppedFailureMarks(fw)
		return st, err
	}
	w.flush()
	st.Resolved += w.resolved
	for _, p := range w.failed {
		if err := ctx.Err(); err != nil {
			st.ReportDroppedFailureMarks(fw)
			return st, err
		}
		st.Failed++
		if fw.markFailed(p.ref, maxResolveAttempts) {
			st.Abandoned++
		}
	}
	// Persist the queued failed-ref markings in one commit; on batch failure
	// each ref falls back to the per-ref update so no failure is lost. A
	// cancelled flush persists nothing, so re-base the counters on what was
	// actually committed before returning the (non-nil) ctx error.
	st.Abandoned += fw.flush(maxResolveAttempts)
	st.ReportDroppedFailureMarks(fw)
	return st, ctx.Err()
}
