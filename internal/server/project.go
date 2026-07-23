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
// by matching query/args against project directory names under any workdir.
// Returns the full path to the detected project, or empty string.
// Results are cached to avoid repeated os.ReadDir + stat per workdir.
func (s *Server) detectProject(queries ...string) string {
	// Check if ANY workdir is broad (home-mode).
	anyBroad := false
	for _, wd := range s.Workdirs {
		if extraction.IsBroadWorkdir(wd) {
			anyBroad = true
			break
		}
	}
	if !anyBroad {
		return ""
	}

	// Populate cached project directories from ALL workdirs.
	s.DetectMu.Lock()
	if !s.DetectDone {
		for _, wd := range s.Workdirs {
			entries, err := os.ReadDir(wd)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				projectDir := filepath.Join(wd, e.Name())
				if extraction.HasProjectMarker(projectDir) {
					s.DetectDirs = append(s.DetectDirs, projectDir)
				}
			}
		}
		s.DetectDone = true
	}
	s.DetectMu.Unlock()

	// Exact match against the base name.
	for _, q := range queries {
		q = strings.ToLower(strings.TrimSpace(q))
		if q == "" {
			continue
		}
		for _, fullPath := range s.DetectDirs {
			base := strings.ToLower(filepath.Base(fullPath))
			if base == q {
				return fullPath
			}
		}
	}

	// Fuzzy: check if any project name appears as a word in the query.
	for _, q := range queries {
		q = strings.ToLower(strings.TrimSpace(q))
		if q == "" {
			continue
		}
		for _, fullPath := range s.DetectDirs {
			base := strings.ToLower(filepath.Base(fullPath))
			if isWordIn(base, q) {
				return fullPath
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
