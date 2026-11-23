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

	w := newEdgeWriterContext(ctx, database)
	fw := newFailureWriterContext(ctx, database)

	// processRef is the shared per-ref body of the two streamed loops below
	// (pending first, then retried-failed — the same order the old
	// pending+retry batch slice produced).
	processRef := func(r db.UnresolvedRef) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		plan, err := s.resolveOne(ctx, r)
		// Cancellation is not a failed reference: do not age/abandon it.
		if cerr := ctx.Err(); cerr != nil {
			return cerr
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
			return nil
		}
		if plan == nil {
			st.Failed++
			if fw.markFailed(r, maxResolveAttempts) {
				st.Abandoned++
			}
			return nil
		}
		w.add(*plan)
		return nil
	}
	finish := func(err error) (Stats, error) {
		st.Resolved += w.resolved
		st.ReportDroppedFailureMarks(fw)
		return st, err
	}

	// Stream the pending backlog in keyset pages instead of materializing
	// the whole []UnresolvedRef (hundreds of MB on large indexes).
	err := database.ForEachUnresolvedRefsContext(ctx, "", "pending", func(r db.UnresolvedRef) error {
		return processRef(r)
	})
	if err != nil {
		return finish(err)
	}
	// Retry failed refs when a candidate name now exists. The candidates
	// lookup is cached per reference_name within this pass: hundreds of
	// thousands of failed rows share a small name space, and the previous
	// per-row query ran once here and again inside resolveOneContext. The
	// failed scan is streamed too — no second full-table slice. fw.seen
	// skips refs this pass already parked as failed: the old snapshot read
	// the failed set BEFORE the pending loop ran, so a pending→failed mark
	// could never be retried in the same pass. The streamed scan would
	// otherwise see those freshly-flipped rows and double-process them.
	err = database.ForEachUnresolvedRefsContext(ctx, "", "failed", func(r db.UnresolvedRef) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := fw.seen[r.ID]; ok {
			return nil
		}
		hit, err := s.hasCandidates(ctx, r.ReferenceName)
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil || !hit {
			return nil
		}
		st.Retried++
		return processRef(r)
	})
	if err != nil {
		return finish(err)
	}
	if err := ctx.Err(); err != nil {
		// Cancellation drops every still-queued mark: ReportedState keeps
		// Failed/Abandoned consistent with rows that were actually persisted.
		return finish(err)
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
	// byName caches ForEachNodeByNameResolveContext results per bare node
	// name: thousands of "x.Close"-style refs share one tail segment, so
	// the per-name SQLite scan runs once per name per pass instead of once
	// per (refName, tail) pair — and a tail-only ref reuses the cached
	// slice by reference instead of paying a fresh copy each time.
	byName map[string][]db.Node
	// storeKeys caches db.StoragePath(workdir, file) per candidate/source
	// file: the import filter computes it once per candidate per ref and
	// the hot paths reuse the same small file-name space.
	storeKeys map[string]string
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
	// probeHits counts cache hits across cand/hasCand so the strided
	// NodeNameExistsContext probe fires every resolveCacheProbeStride hits.
	probeHits int
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
		database:  database,
		workdir:   workdir,
		cand:      map[string][]db.Node{},
		byName:    map[string][]db.Node{},
		storeKeys: map[string]string{},
		hasCand:   map[string]bool{},
		imported:  map[importedKey]*importClosure{},
	}
}

// resolveCacheProbeStride is how often a cache HIT in candidates/
// hasCandidates still performs a real NodeNameExistsContext SQL probe. The
// un-cached path ran one query per ref; after the per-pass caches landed,
// every cached hit ran the probe so a cancellation fired inside SQL kept
// landing on the NEXT ref — hundreds of thousands of cache hits meant
// ~1.1M probe round trips on a large backlog. The stride keeps that SQL
// cancellation surface (every Nth hit still probes, and ctx.Err() is
// checked per ref in the loop anyway — a pure in-memory hit can never
// block), while collapsing the round trips to ~hits/stride. A var so tests
// can restore the every-hit probe (stride=1) where their assertions count
// the probe calls.
var resolveCacheProbeStride = 4096

// candidates returns the candidate node set for a reference name. The first
// call per name runs the full collectCandidatesContext query and stores the
// result; later calls for the same name replay it. Cache hits still run the
// real NodeNameExistsContext probe once per resolveCacheProbeStride hits so
// a cancellation fired inside SQL lands on a later ref — never on a value
// replayed out of the map.
func (s *resolveScope) candidates(ctx context.Context, refName string) ([]db.Node, error) {
	refName = strings.TrimSpace(refName)
	if v, ok := s.cand[refName]; ok {
		s.probeHits++
		if s.probeHits%resolveCacheProbeStride == 0 {
			if _, err := s.database.NodeNameExistsContext(ctx, refName); err != nil {
				return nil, err
			}
		}
		return v, nil
	}
	if refName == "" {
		// collectCandidatesContext's contract: empty names yield no
		// candidates; the nil result is cached like any other miss.
		s.cand[refName] = nil
		return nil, nil
	}
	exact, err := s.nodesByName(ctx, refName)
	if err != nil {
		return nil, err
	}
	var nodes []db.Node
	if tail := nameTail(refName); tail != "" && tail != refName {
		// Class.method / pkg.Func / util.greet: merge the tail's nodes after
		// the exact-name set (same order and ID-dedup as
		// collectCandidatesContext).
		tailNodes, terr := s.nodesByName(ctx, tail)
		if terr != nil {
			return nil, terr
		}
		switch {
		case len(exact) == 0:
			// Share the cached tail slice by reference — candidates are only
			// ever read (the self-ref filter copies on write), so thousands
			// of tail-only refs (x.Close, t.Fatalf, ...) reuse one backing
			// array instead of each paying a fresh SQLite scan + copy.
			nodes = tailNodes
		case len(tailNodes) == 0:
			nodes = exact
		default:
			seen := make(map[int64]bool, len(exact)+len(tailNodes))
			nodes = make([]db.Node, 0, len(exact)+len(tailNodes))
			for _, n := range exact {
				if !seen[n.ID] {
					seen[n.ID] = true
					nodes = append(nodes, n)
				}
			}
			for _, n := range tailNodes {
				if !seen[n.ID] {
					seen[n.ID] = true
					nodes = append(nodes, n)
				}
			}
		}
	} else {
		nodes = exact
	}
	s.cand[refName] = nodes
	return nodes, nil
}

// nodesByName returns the resolve-projection nodes carrying exactly name,
// cached per bare name for the pass — the resolution pass only writes
// edges/unresolved_refs and never nodes, so a cached list stays valid for
// the whole pass. Errors are NOT cached: each ref keeps the old per-row
// retry behavior, and ctx cancellation must re-fire for the next caller.
func (s *resolveScope) nodesByName(ctx context.Context, name string) ([]db.Node, error) {
	if name == "" {
		return nil, nil
	}
	if v, ok := s.byName[name]; ok {
		return v, nil
	}
	var nodes []db.Node
	err := s.database.ForEachNodeByNameResolveContext(ctx, name, func(n db.Node) error {
		nodes = append(nodes, n)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.byName[name] = nodes
	return nodes, nil
}

// storeKey returns the workdir-relative storage key for file, cached per
// file for the pass — the import filter otherwise recomputes
// db.StoragePath (filepath.Clean + Rel + ToSlash) once per candidate per
// ref over the same small file-name space.
func (s *resolveScope) storeKey(file string) string {
	if v, ok := s.storeKeys[file]; ok {
		return v
	}
	k := db.StoragePath(s.workdir, file)
	s.storeKeys[file] = k
	return k
}

// hasCandidates runs the failed-retry existence check for a reference
// name, with the same strided probe query as candidates(): every
// resolveCacheProbeStride'th cache hit touches SQL once so cancellation
// inside SQL still lands on a later ref.
func (s *resolveScope) hasCandidates(ctx context.Context, refName string) (bool, error) {
	refName = strings.TrimSpace(refName)
	if v, ok := s.hasCand[refName]; ok {
		s.probeHits++
		if s.probeHits%resolveCacheProbeStride == 0 {
			if _, err := s.database.NodeNameExistsContext(ctx, refName); err != nil {
				return false, err
			}
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

	candidates, err = s.dropSelfRef(ctx, candidates, r.FromNode)
	if err != nil {
		return nil, err
	}
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
	if imp, ok := s.filterByImportedFiles(ctx, closure, candidates); ok {
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
	if m.TargetID == 0 && preferCall {
		m, err = s.uniqueCallableMatch(ctx, closure, candidates, fromFile)
		if err != nil {
			return nil, err
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

// dropSelfRef removes the caller's own node from candidates (never link a
// node to itself). Most candidate lists contain no self-reference, so this
// scans first and only allocates a filtered copy when one is actually
// present — filtering in place or copying eagerly would either corrupt
// s.cand's shared array or pay a full copy per ref.
func (s *resolveScope) dropSelfRef(ctx context.Context, candidates []db.Node, fromNode int64) ([]db.Node, error) {
	hasSelf := false
	for _, c := range candidates {
		if c.ID == fromNode {
			hasSelf = true
			break
		}
	}
	if !hasSelf {
		return candidates, nil
	}
	filtered := make([]db.Node, 0, len(candidates)-1)
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c.ID != fromNode {
			filtered = append(filtered, c)
		}
	}
	return filtered, nil
}

// uniqueCallableMatch accepts a sole global callable candidate as a
// heuristic match, provided it is backed by import closure, same-directory,
// or direct-subdirectory proximity (the candidate lives inside a
// subdirectory of the caller's directory). A non-unique callable set yields
// an empty MatchResult. Cancellation is checked per candidate scan.
func (s *resolveScope) uniqueCallableMatch(ctx context.Context, closure *importClosure, candidates []db.Node, fromFile string) (MatchResult, error) {
	callableCount := 0
	var onlyCallable db.Node
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return MatchResult{}, err
		}
		if callTargetKinds[c.Kind] {
			callableCount++
			if callableCount == 1 {
				onlyCallable = c
			} else {
				return MatchResult{}, nil // not unique — no single callable to accept
			}
		}
	}
	if callableCount != 1 {
		return MatchResult{}, nil
	}
	cdir := filepath.Dir(onlyCallable.File)
	fromDir := filepath.Dir(fromFile)
	if _, ok := s.filterByImportedFiles(ctx, closure, []db.Node{onlyCallable}); ok {
		return MatchResult{TargetID: onlyCallable.ID, Provenance: ProvImport}, nil
	}
	if cdir == fromDir || filepath.Dir(cdir) == fromDir {
		return MatchResult{TargetID: onlyCallable.ID, Provenance: ProvHeuristic}, nil
	}
	return MatchResult{}, nil
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
	// queuedAbandoned counts how many still-queued marks would already reach
	// the abandon cap, computed against the DB's CURRENT attempts value at
	// queue time. These are still only attempts: the DB row is unchanged
	// until flush commits, so Stats must never report them as abandoned.
	// ReportDroppedFailureMarks re-bases Stats.Abandoned straight onto
	// persistedAbandoned, so this field is bookkeeping for flushChunk's
	// rebase only — kept so a cancelled mid-fallback flush still separates
	// the committed prefix's abandoned rows from the unstarted tail's.
	queuedAbandoned int
	// persistedAbandoned is how many queuedAbandoned rows flush actually
	// committed as 'abandoned' — the running total across every flush this
	// pass (each mid-pass flushChunk AND the final flush). Stats.Abandoned
	// must equal this after any early finish: chunks that already committed
	// keep their rows even when a later cancel aborts the pass.
	persistedAbandoned int
	// seen records every ref id markFailed was called for in this pass so
	// the streamed failed scan can skip rows this pass itself just parked
	// — the old snapshot-based read could never see them (it was taken
	// before the pending loop).
	seen map[int64]struct{}
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
// so flushChunk can keep the prediction scoped to the still-queued marks
// after a partially-committed cancelled flush.
func (fw *failureWriter) markFailed(r db.UnresolvedRef, maxAttempts int) bool {
	tail := r.NameTail
	if tail == "" {
		tail = nameTail(r.ReferenceName)
	}
	// Predict abandonment from the row's CURRENT attempts (pre-write), read
	// off the same ListUnresolvedRefs* scan that produced the ref — the old
	// per-ref UnresolvedRefAttempts point query ran once per markFailed call
	// (hundreds of thousands of extra round trips on a large backlog). The
	// row keeps its pre-write attempts value until flush: the only writer of
	// attempts inside a pass is this writer's own flush, which happens after
	// all marks are queued, so the value read at list time IS the pre-write
	// value every mark predicts against (same contract as before).
	if r.Attempts+1 >= maxAttempts {
		fw.queuedAbandoned++
	}
	fw.marks = append(fw.marks, failedMark{ref: r, tail: tail})
	if fw.seen == nil {
		fw.seen = map[int64]struct{}{}
	}
	fw.seen[r.ID] = struct{}{}
	if len(fw.marks) >= failureFlushChunk {
		fw.flushChunk(maxAttempts)
	}
	return false
}

// flush persists every queued marking in one transaction and returns how
// many refs it parked as abandoned. On batch failure it retries each ref
// through the old per-ref path (MarkUnresolvedFailed) so one bad row cannot
// lose the rest, matching edgeWriter's isolation contract. Cancellation is
// checked once up front (like edgeWriter.flush): a cancelled pass leaves
// queued refs unmarked so they are retried, never aged — in that case the
// queued tail is dropped, persistedAbandoned stays 0, and the caller must
// report the dropped marks so Stats stays consistent with the DB.
//
// Queue discipline: fw.marks only ever holds UNPERSISTED marks — the batch
// commit consumes the whole queue (marks reset to len 0), and the per-ref
// fallback path consumes each mark it commits and each mark the context
// check proves it can no longer start, leaving a cancelled flush with the
// tail it never reached. A mark is removed the moment it is committed, so
// droppedMarks()'s len(fw.marks) always equals the un-persisted count.
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
		// Per-ref fallback. Each iteration's ctx check fires BEFORE that
		// ref's commit starts, so a mark lands iff the context allowed it —
		// truncate below re-queues exactly the marks that never began.
		k := 0 // count of prefix marks committed
		for _, m := range fw.marks {
			if fw.ctx.Err() != nil {
				fw.marks = append(fw.marks[:0], fw.marks[k:]...)
				fw.persistedAbandoned += n
				return n
			}
			if markRefFailed(fw.database, m.ref, maxAttempts) {
				n++
			}
			k++
		}
		fw.marks = fw.marks[:0]
		fw.persistedAbandoned += n
		return n
	}
	n := 0
	for _, a := range abandoned {
		if a {
			n++
		}
	}
	fw.marks = fw.marks[:0]
	fw.persistedAbandoned += n
	return n
}

// failureFlushChunk is how many queued failed marks markFailed flushes
// into one ApplyUnresolvedFailed transaction before continuing — an
// unbounded fw.marks slice dominated peak RSS on a large all-fail backlog
// (one mid-pass commit also shrinks the window a crash/cancel would drop,
// matching the per-ref era's worst case rather than the whole tail).
// queuedAbandoned tracks only the still-unflushed marks (see flushChunk),
// so Stats stays consistent whether a pass commits zero, some, or every
// chunk. A var so tests can shrink it.
var failureFlushChunk = 50_000

// flushChunk persists the marks queued so far as ONE chunk transaction.
// queuedAbandoned tracks only the still-unflushed marks, so after a chunk
// commit it is rebased to the queue that survived (empty on a full commit;
// the unstarted tail on a cancelled mid-fallback flush) — droppedMarks must
// count just those, or a cancelled pass would subtract rows a committed
// chunk already landed and under-report Failed/Abandoned against the real
// DB state.
func (fw *failureWriter) flushChunk(maxAttempts int) {
	if len(fw.marks) == 0 {
		return
	}
	queued := fw.queuedAbandoned
	n := fw.flush(maxAttempts)
	if len(fw.marks) == 0 {
		// Whole queue committed: nothing left to predict.
		fw.queuedAbandoned = 0
		return
	}
	// Cancellation aborted the flush part-way: n rows parked as abandoned
	// committed into persistedAbandoned; the unstarted tail keeps only
	// queued−n uncommitted predicted-abandoned marks.
	fw.queuedAbandoned = queued - n
}

// droppedMarks is how many queued failure markings were never persisted.
// Queue discipline (see flush) guarantees fw.marks holds ONLY unpersisted
// marks: every committed chunk or fallback prefix is truncated out of
// fw.marks the moment it lands, so len(fw.marks) is exactly the count still
// owed to the DB — whether the pass is about to run its final flush, bailed
// on cancellation, or died on a mid-scan SQL error (the previous
// ctx.Err() gate wrongly treated a non-cancel early finish as
// "nothing dropped" and left those marks counted in Stats.Failed).
// edgeWriter-buffered plans that surface as w.failed are counted by the
// caller before queueing them here, so this only needs to cover marks that
// reached markFailed.
func (fw *failureWriter) droppedMarks() int {
	return len(fw.marks)
}

// ReportDroppedFailureMarks re-bases Failed/Abandoned on what the failure
// writer actually persisted, on every finish path — cancelled or not.
// markFailed increments Stats.Failed for every queued row but reports
// abandoned predictively; the queue discipline means len(fw.marks) is
// exactly the marks never committed, and persistedAbandoned already tracks
// the abandoned rows every flush (mid-pass chunk or final) committed — so
// Stats is simply re-based onto the persisted totals instead of computed
// from prediction deltas (the old `queuedAbandoned - persistedAbandoned`
// subtraction could go negative and silently drop the count of chunks
// that had already committed).
func (st *Stats) ReportDroppedFailureMarks(fw *failureWriter) {
	st.Failed -= fw.droppedMarks()
	st.Abandoned = fw.persistedAbandoned
	if st.Failed < 0 {
		st.Failed = 0
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
