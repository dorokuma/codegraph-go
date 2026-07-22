package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/internal/extraction"
)

// dbEntry wraps a cached project DB with a reference count so concurrent
// tool calls don't race with LRU eviction.
type dbEntry struct {
	db   *db.DB
	refs int32 // atomic: number of in-flight callers using this DB
}

// detectProject tries to find which project the user is asking about
// by matching query/args against project directory names under Workdir.
// Returns the project dir name (relative to Workdir) or empty string.
// Results are cached per Workdir to avoid repeated os.ReadDir + stat.
func (s *Server) detectProject(queries ...string) string {
	if !extraction.IsBroadWorkdir(s.Workdir) {
		return ""
	}
	// Populate cached project names; retry on failure (unlike sync.Once).
	s.DetectMu.Lock()
	if !s.DetectDone {
		entries, err := os.ReadDir(s.Workdir)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				projectDir := filepath.Join(s.Workdir, e.Name())
				if extraction.HasProjectMarker(projectDir) {
					s.DetectDirs = append(s.DetectDirs, e.Name())
				}
			}
			s.DetectDone = true
		}
		// On failure, don't set DetectDone — retry next call
	}
	s.DetectMu.Unlock()
	for _, q := range queries {
		q = strings.ToLower(strings.TrimSpace(q))
		if q == "" {
			continue
		}
		for _, name := range s.DetectDirs {
			if strings.ToLower(name) == q {
				return name
			}
		}
	}
	// Fuzzy: check if any project name appears as a word in the query
	for _, q := range queries {
		q = strings.ToLower(strings.TrimSpace(q))
		if q == "" {
			continue
		}
		for _, name := range s.DetectDirs {
			if isWordIn(strings.ToLower(name), q) {
				return name
			}
		}
	}
	return ""
}

// resetDetect clears the detect cache so the next detectProject call rescans
// Workdir. Used when a projectPath lookup fails, indicating the cache may be stale.
func (s *Server) resetDetect() {
	s.DetectMu.Lock()
	s.DetectDone = false
	s.DetectDirs = nil
	s.DetectMu.Unlock()
}

// resolveProject picks the DB + root for a tool call.
// Empty projectPath → session default. Non-empty → walk up to nearest .codegraph/.
func (s *Server) resolveProject(projectPath string) (root string, database *db.DB, err error) {
	if strings.TrimSpace(projectPath) == "" {
		return s.Workdir, s.Database, nil
	}
	abs, err := filepath.Abs(projectPath)
	if err != nil {
		return "", nil, fmt.Errorf("bad projectPath %q: %w", projectPath, err)
	}
	resolved := db.FindNearestCodeGraphRoot(abs)
	if resolved == "" {
		// Cache may be stale — allow detectProject to rescan on next call.
		s.resetDetect()
		return "", nil, fmt.Errorf(
			"no .codegraph index found walking up from %s; pass a path inside an indexed project, or omit projectPath to use the session default",
			abs,
		)
	}
	if resolved == s.Workdir {
		return s.Workdir, s.Database, nil
	}
	s.ProjectMu.Lock()
	defer s.ProjectMu.Unlock()
	if s.ProjectCache == nil {
		s.ProjectCache = map[string]*dbEntry{}
		s.ProjectPendingClose = map[string]*dbEntry{}
		s.ProjectMaxLRU = 10 // keep at most 10 cross-project DBs open
	}
	if cached, ok := s.ProjectCache[resolved]; ok {
		// Move to end of LRU list (most recently used).
		s.touchProjectLRU(resolved)
		atomic.AddInt32(&cached.refs, 1)
		return resolved, cached.db, nil
	}
	// Evict oldest if at capacity. For entries still in use (refs>0),
	// remove from cache+LRU but defer Close to releaseProject via
	// ProjectPendingClose so in-flight callers are not disrupted.
	if s.ProjectMaxLRU > 0 && len(s.ProjectLRU) >= s.ProjectMaxLRU {
		evict := s.ProjectLRU[0]
		s.ProjectLRU = s.ProjectLRU[1:]
		if e, ok := s.ProjectCache[evict]; ok {
			delete(s.ProjectCache, evict)
			if atomic.LoadInt32(&e.refs) == 0 {
				_ = e.db.Close()
			} else {
				s.ProjectPendingClose[evict] = e
			}
		}
	}
	opened, err := db.Open(resolved)
	if err != nil {
		return "", nil, fmt.Errorf("open index at %s: %w", resolved, err)
	}
	entry := &dbEntry{db: opened, refs: 1}
	s.ProjectCache[resolved] = entry
	s.ProjectLRU = append(s.ProjectLRU, resolved)
	return resolved, opened, nil
}

// touchProjectLRU moves root to the end of the LRU list (must hold ProjectMu).
func (s *Server) touchProjectLRU(root string) {
	for i, r := range s.ProjectLRU {
		if r == root {
			s.ProjectLRU = append(s.ProjectLRU[:i], s.ProjectLRU[i+1:]...)
			s.ProjectLRU = append(s.ProjectLRU, root)
			return
		}
	}
}

// releaseProject decrements the refcount on a cross-project DB and closes
// it when the count reaches zero AND the entry has been evicted from the
// cache (pending-close). For the session-default DB this is a no-op.
func (s *Server) releaseProject(root string) {
	if root == s.Workdir {
		return
	}
	s.ProjectMu.Lock()
	defer s.ProjectMu.Unlock()
	if e, ok := s.ProjectPendingClose[root]; ok {
		if atomic.AddInt32(&e.refs, -1) == 0 {
			_ = e.db.Close()
			delete(s.ProjectPendingClose, root)
		}
		return
	}
	if e, ok := s.ProjectCache[root]; ok {
		atomic.AddInt32(&e.refs, -1)
	}
}
