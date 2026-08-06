package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/dorokuma/codegraph-go/internal/config"
	"github.com/dorokuma/codegraph-go/internal/daemon"
	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/internal/extraction"
	"github.com/dorokuma/codegraph-go/internal/sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server holds the shared state for the MCP server: DB, orchestrator, watcher,
// and cross-project cache.
type Server struct {
	Workdir      string
	Workdirs     []string
	Database     *db.DB
	Orchestrator *extraction.Orchestrator
	// Watcher is set from the background index goroutine after auto-sync starts.
	Watcher atomic.Pointer[sync.Watcher]

	// BgDone signals the background index/watch goroutine to exit.
	BgDone chan struct{}
	BgWg   stdsync.WaitGroup

	// Cross-project cache: resolved .codegraph root → open DB with ref-counting
	// so concurrent tool calls don't race with LRU eviction (W-1).
	ProjectMu           stdsync.Mutex
	ProjectCache        map[string]*dbEntry // guarded by ProjectMu
	ProjectLRU          []string            // ordered by access time; oldest first
	ProjectMaxLRU       int                 // max cached project DBs (0 = unlimited)
	ProjectPendingClose map[string]*dbEntry // evicted but still in use; guarded by ProjectMu

	// DefReCache avoids recompiling the caller-filter regex per toolCallers invocation.
	DefReCache stdsync.Map // string → *regexp.Regexp

	// pathMu guards realRoots (B6: EvalSymlinks cache for resolvePathIn).
	pathMu    stdsync.Mutex
	realRoots map[string]string // workspace root → symlink-resolved real path

	// detectCache avoids repeated os.ReadDir+stat per tool call in home mode.
	DetectMu   stdsync.Mutex
	DetectDone bool
	DetectDirs []string // cached project directory names under Workdir

	// Extra workdirs: keep DB open + file watcher so secondary roots stay fresh
	// (primary uses Database / Watcher above). Keyed by absolute workdir path.
	// resolveProject reuses these handles so tools never open a second writer.
	ExtraMu       stdsync.Mutex
	ExtraDBs      map[string]*db.DB
	ExtraWatchers map[string]*sync.Watcher
}

// onceRWC closes the underlying ReadWriteCloser at most once.
type onceRWC struct {
	io.ReadWriteCloser
	once stdsync.Once
	err  error
}

func (o *onceRWC) Close() error {
	o.once.Do(func() { o.err = o.ReadWriteCloser.Close() })
	return o.err
}

// RunDirect starts a single-process MCP server (stdio) from config.
// It opens the server state and runs until the transport closes.
func RunDirect(cfg config.Config) error {
	s, cleanup := OpenServerState(cfg.Workdir, cfg.Workdirs, cfg.NoSync)
	defer cleanup()

	srv := NewMCPServer(s)
	// PPID watchdog (B3): cancel the session context so srv.Run returns and
	// the deferred cleanup runs (WAL checkpoint stop, connection close) —
	// no os.Exit, so defers are honored.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopWD := daemon.StartPPIDWatchdog(daemon.PPIDPollInterval(), func() {
		cancel()
	})
	defer stopWD()

	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		if errors.Is(err, context.Canceled) {
			// Parent died — graceful shutdown, not a server error.
			return nil
		}
		return fmt.Errorf("server exited: %w", err)
	}
	return nil
}

// RunDaemonProcess starts a daemon-mode MCP server that accepts
// connections via UNIX socket. It is the CODEGRAPH_DAEMON_INTERNAL entry point.
func RunDaemonProcess(cfg config.Config) error {
	var (
		stateOnce stdsync.Once
		s         *Server
		cleanup   func()
		mcpSrv    *mcp.Server
	)
	ensure := func() {
		stateOnce.Do(func() {
			s, cleanup = OpenServerState(cfg.Workdir, cfg.Workdirs, cfg.NoSync)
			mcpSrv = NewMCPServer(s)
		})
	}
	handler := func(ctx context.Context, rwc io.ReadWriteCloser) error {
		ensure()
		// Each connection is one MCP session sharing tools/DB/watcher.
		// IOTransport closes Reader and Writer separately — once-wrap so the
		// underlying conn is closed exactly once.
		oc := &onceRWC{ReadWriteCloser: rwc}
		ss, err := mcpSrv.Connect(ctx, &mcp.IOTransport{
			Reader: oc,
			Writer: oc,
		}, nil)
		if err != nil {
			return err
		}
		return ss.Wait()
	}
	onReady := func() error {
		ensure()
		return nil
	}
	if err := daemon.RunAsDaemon(cfg.Workdir, handler, onReady); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return fmt.Errorf("daemon: %w", err)
	}
	if cleanup != nil {
		cleanup()
	}
	return nil
}

// OpenServerState opens DB + orchestrator and kicks background index/watcher.
// workdir is the primary workspace root (first in the workdirs list) for
// backward compatibility; workdirs is the full list of workspace roots.
func OpenServerState(workdir string, workdirs []string, noSync bool) (*Server, func()) {
	if workdirs == nil {
		workdirs = []string{workdir}
	}
	database, err := db.Open(workdir)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}

	// Start WAL checkpoint background loop (every 5 minutes).
	walCP := db.NewWALCheckpoint(database, 5*time.Minute)
	walCP.Start()

	orch := extraction.NewOrchestrator(database, workdir)
	s := &Server{
		Workdir:      workdir,
		Workdirs:     workdirs,
		Database:     database,
		Orchestrator: orch,
		BgDone:       make(chan struct{}),
	}
	// Shutdown interrupt for the index loops: when Stop closes BgDone the
	// in-flight IndexAll/RebuildAll/IndexChanges pass winds down within one
	// unit of work instead of blocking BgWg.Wait() for the whole workspace.
	orch.SetDone(s.BgDone)
	s.BgWg.Add(1)
	go backgroundIndexAndWatch(s, noSync)
	cleanup := func() {
		// Signal background goroutine to stop, then wait for it to finish
		// before closing the database.
		close(s.BgDone)
		s.BgWg.Wait()
		walCP.Stop()
		if w := s.Watcher.Load(); w != nil {
			w.Stop()
		}
		s.ExtraMu.Lock()
		for _, w := range s.ExtraWatchers {
			if w != nil {
				w.Stop()
			}
		}
		for _, edb := range s.ExtraDBs {
			if edb != nil {
				_ = edb.Close()
			}
		}
		s.ExtraWatchers = nil
		s.ExtraDBs = nil
		s.ExtraMu.Unlock()
		s.closeProjectCache()
		_ = database.Close()
	}
	return s, cleanup
}

func backgroundIndexAndWatch(s *Server, noSync bool) {
	defer s.BgWg.Done()
	database := s.Database
	orch := s.Orchestrator
	workdir := s.Workdir

	// --- Primary workdir indexing ---
	select {
	case <-s.BgDone:
		return
	default:
	}

	var files, nodes int
	var err error
	rebuild, oldVer, rebuildErr := database.NeedsRebuild()
	if rebuildErr != nil {
		// B2 (Critical#2 server side): schema state is unknown — e.g. the DB
		// is locked by another process or the meta table is unreadable.
		// Rebuilding or indexing now could double-write or corrupt the
		// index, so log the error and skip this round entirely: no
		// RebuildAll, no IndexAll, no SetSchemaRevision. The watcher below
		// still re-syncs on later changes.
		slog.Error("schema revision check failed; skipping index round", "error", rebuildErr)
	} else {
		if rebuild {
			slog.Info("full rebuild", "from", oldVer, "to", db.SchemaRevision())

			select {
			case <-s.BgDone:
				return
			default:
			}
			files, nodes, err = orch.RebuildAll()
			// RebuildAll marks the schema revision only on a complete pass.
			// An interrupted rebuild returns ErrIndexInterrupted and leaves
			// the revision at its old value: the next startup sees
			// NeedsRebuild()==true and re-runs the full rebuild, so a wiped
			// half index is never presented as current.
		} else {
			slog.Info("indexing project in background...")

			select {
			case <-s.BgDone:
				return
			default:
			}
			files, nodes, err = orch.IndexAll()
			if errors.Is(err, extraction.ErrIndexInterrupted) {
				// Shutdown interrupted the pass: do NOT mark the schema
				// revision — the half index must not masquerade as current.
				// Logged as an interruption below, not as success. No retry:
				// BgDone is closed, so every later step in this goroutine
				// bails on it.
			} else if err == nil {
				_ = database.SetSchemaRevision()
			}
		}
		if err != nil {
			if errors.Is(err, extraction.ErrIndexInterrupted) {
				slog.Warn("index pass interrupted by shutdown, revision not marked")
			} else {
				slog.Warn("index warning", "error", err)
			}
		} else {
			slog.Info("indexed primary", "files", files, "nodes", nodes, "schema", db.SchemaRevision())
		}

		// Optional git-status assist: catch edits missed while nothing was watching.
		if dirty := sync.GitDirtySourceFiles(workdir); len(dirty) > 0 {
			select {
			case <-s.BgDone:
				return
			default:
			}
			c, n, gerr := orch.IndexChanges(dirty)
			if gerr != nil {
				if errors.Is(gerr, extraction.ErrIndexInterrupted) {
					slog.Warn("index pass interrupted by shutdown, revision not marked")
				} else {
					slog.Warn("git-assist sync", "error", gerr)
				}
			} else if c > 0 {
				slog.Info("git-assist sync", "files", c, "nodes", n)
			}
		}
	}

	// --- Additional workdirs indexing (open DB, index, close) ---
	for i, wd := range s.Workdirs {
		if i == 0 {
			continue // primary already indexed
		}
		select {
		case <-s.BgDone:
			return
		default:
		}

		otherDB, oerr := db.Open(wd)
		if oerr != nil {
			slog.Warn("open additional workdir DB", "workdir", wd, "error", oerr)
			continue
		}
		otherOrch := extraction.NewOrchestrator(otherDB, wd)
		otherOrch.SetDone(s.BgDone)
		rebuildNeeded, oldVer, rerr := otherDB.NeedsRebuild()
		if rerr != nil {
			slog.Warn("schema check", "workdir", wd, "error", rerr)
			otherDB.Close()
			continue
		}
		var f2, n2 int
		if rebuildNeeded {
			slog.Info("full rebuild", "workdir", wd, "from", oldVer, "to", db.SchemaRevision())
			f2, n2, err = otherOrch.RebuildAll()
		} else {
			slog.Info("indexing", "workdir", wd)
			f2, n2, err = otherOrch.IndexAll()
			if errors.Is(err, extraction.ErrIndexInterrupted) {
				// Same rule as the primary workdir: never mark the revision
				// on an interrupted pass — the next startup rebuilds it.
			} else if err == nil {
				_ = otherDB.SetSchemaRevision()
			}
		}
		if err != nil {
			if errors.Is(err, extraction.ErrIndexInterrupted) {
				slog.Warn("index pass interrupted by shutdown, revision not marked", "workdir", wd)
			} else {
				slog.Warn("index warning", "workdir", wd, "error", err)
			}
			_ = otherDB.Close()
			continue
		}
		slog.Info("indexed", "workdir", wd, "files", f2, "nodes", n2)

		// Keep secondary DB open and watch for changes (parity with primary).
		if !noSync {
			select {
			case <-s.BgDone:
				_ = otherDB.Close()
				return
			default:
			}
			w2, wErr := sync.NewWatcher(otherOrch, wd)
			if wErr != nil {
				slog.Warn("watcher warning", "workdir", wd, "error", wErr)
				_ = otherDB.Close()
				continue
			}
			if wErr := w2.Start(); wErr != nil {
				slog.Warn("watcher start warning", "workdir", wd, "error", wErr)
				_ = otherDB.Close()
				continue
			}
			s.ExtraMu.Lock()
			if s.ExtraDBs == nil {
				s.ExtraDBs = map[string]*db.DB{}
			}
			if s.ExtraWatchers == nil {
				s.ExtraWatchers = map[string]*sync.Watcher{}
			}
			// Drop any stale cross-project cache handle for this root so we
			// never keep two open connections to the same secondary index.
			s.ProjectMu.Lock()
			if e, ok := s.ProjectCache[wd]; ok {
				delete(s.ProjectCache, wd)
				s.removeProjectLRU(wd)
				if atomic.LoadInt32(&e.refs) == 0 {
					_ = e.db.Close()
				} else {
					if s.ProjectPendingClose == nil {
						s.ProjectPendingClose = map[string]*dbEntry{}
					}
					s.ProjectPendingClose[wd] = e
				}
			}
			s.ProjectMu.Unlock()
			s.ExtraDBs[wd] = otherDB
			s.ExtraWatchers[wd] = w2
			s.ExtraMu.Unlock()
			slog.Info("auto-sync enabled", "workdir", wd)
		} else {
			_ = otherDB.Close()
		}
	}

	// --- Watcher (primary workdir) ---
	if noSync {
		return
	}

	select {
	case <-s.BgDone:
		return
	default:
	}
	watcher, wErr := sync.NewWatcher(orch, workdir)
	if wErr != nil {
		slog.Warn("watcher warning", "error", wErr)
		return
	}
	if wErr := watcher.Start(); wErr != nil {
		slog.Warn("watcher start warning", "error", wErr)
		return
	}
	s.Watcher.Store(watcher)
	slog.Info("auto-sync enabled", "workdir", workdir)
}

// closeProjectCache closes all cached cross-project DBs and pending-close
// entries at shutdown. Entries still in use (refs>0) are left for OS cleanup.
func (s *Server) closeProjectCache() {
	s.ProjectMu.Lock()
	defer s.ProjectMu.Unlock()
	for root, e := range s.ProjectCache {
		if atomic.LoadInt32(&e.refs) > 0 {
			continue
		}
		_ = e.db.Close()
		delete(s.ProjectCache, root)
	}
	for root, e := range s.ProjectPendingClose {
		if atomic.LoadInt32(&e.refs) > 0 {
			continue
		}
		_ = e.db.Close()
		delete(s.ProjectPendingClose, root)
	}
	s.ProjectLRU = nil
}
