package resolution

import (
	"log"
	"strings"

	"github.com/dorokuma/codegraph-go/db"
)

// Stats summarizes one ResolveAll pass.
type Stats struct {
	Resolved int
	Failed   int
	Retried  int
}

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
		tail := r.NameTail
		if tail == "" {
			tail = nameTail(r.ReferenceName)
		}
		cands, err := CollectCandidates(database, r.ReferenceName)
		if err != nil || len(cands) == 0 {
			continue
		}
		retry = append(retry, r)
	}
	st.Retried = len(retry)

	batch := append(pending, retry...)
	for _, r := range batch {
		ok, err := resolveOne(database, workdir, r)
		if err != nil {
			log.Printf("resolve ref %s: %v", r.ReferenceName, err)
			continue
		}
		if ok {
			st.Resolved++
		} else {
			st.Failed++
			tail := r.NameTail
			if tail == "" {
				tail = nameTail(r.ReferenceName)
			}
			_ = database.MarkUnresolvedFailed(r.ID, tail)
		}
	}
	return st, nil
}

func resolveOne(database *db.DB, workdir string, r db.UnresolvedRef) (bool, error) {
	if r.FromNode == 0 || r.ReferenceName == "" {
		return false, nil
	}
	kind := r.ReferenceKind
	if kind == "" {
		kind = db.EdgeCalls
	}
	preferCall := kind == db.EdgeCalls || kind == db.EdgeReferences || kind == "bridge"

	candidates, err := CollectCandidates(database, r.ReferenceName)
	if err != nil {
		return false, err
	}
	if len(candidates) == 0 {
		return false, nil
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
		return false, nil
	}

	lang := r.Language
	fromFile := r.FilePath

	// Import-closure preference
	if imp, ok := FilterByImports(database, workdir, fromFile, lang, candidates); ok {
		m := MatchName(imp, r.ReferenceName, fromFile, preferCall)
		if m.TargetID != 0 {
			return writeEdge(database, r, m.TargetID, kind, ProvImport)
		}
	}

	m := MatchName(candidates, r.ReferenceName, fromFile, preferCall)
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
				m = MatchResult{TargetID: callables[0].ID, Provenance: ProvHeuristic}
			}
		}
	}
	if m.TargetID == 0 {
		return false, nil
	}
	if m.Provenance == "" {
		m.Provenance = ProvHeuristic
	}
	return writeEdge(database, r, m.TargetID, kind, m.Provenance)
}

func writeEdge(database *db.DB, r db.UnresolvedRef, targetID int64, kind, provenance string) (bool, error) {
	if _, err := database.UpsertEdge(&db.Edge{
		SourceID:   r.FromNode,
		TargetID:   targetID,
		Kind:       kind,
		File:       r.FilePath,
		Line:       r.Line,
		Col:        r.Col,
		Provenance: provenance,
	}); err != nil {
		return false, err
	}
	if err := database.DeleteUnresolvedRef(r.ID); err != nil {
		return false, err
	}
	return true, nil
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
	pending, err := database.ListUnresolvedRefs("", "pending")
	if err != nil {
		return st, err
	}
	failed, err := database.ListUnresolvedRefs("", "failed")
	if err != nil {
		return st, err
	}
	var batch []db.UnresolvedRef
	for _, r := range pending {
		if want[r.FilePath] {
			batch = append(batch, r)
		}
	}
	// Also any pending/failed whose name might be defined in changed files.
	changedNames := map[string]bool{}
	for _, f := range files {
		nodes, err := database.GetNodesByFile(f)
		if err != nil {
			continue
		}
		for _, n := range nodes {
			changedNames[n.Name] = true
		}
	}
	for _, r := range append(pending, failed...) {
		if want[r.FilePath] {
			continue // already queued
		}
		tail := r.NameTail
		if tail == "" {
			tail = nameTail(r.ReferenceName)
		}
		if changedNames[r.ReferenceName] || changedNames[tail] {
			batch = append(batch, r)
			if r.Status == "failed" {
				st.Retried++
			}
		}
	}
	// Dedupe by id
	seen := map[int64]bool{}
	for _, r := range batch {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		ok, err := resolveOne(database, workdir, r)
		if err != nil {
			log.Printf("resolve ref %s: %v", r.ReferenceName, err)
			continue
		}
		if ok {
			st.Resolved++
		} else if r.Status == "pending" || r.Status == "" {
			st.Failed++
			tail := r.NameTail
			if tail == "" {
				tail = nameTail(r.ReferenceName)
			}
			_ = database.MarkUnresolvedFailed(r.ID, tail)
		}
	}
	return st, nil
}

// preferCallKind reports whether a reference kind wants call-target filtering.
func preferCallKind(kind string) bool {
	return kind == db.EdgeCalls || kind == db.EdgeReferences || kind == "bridge" ||
		strings.EqualFold(kind, "calls")
}
