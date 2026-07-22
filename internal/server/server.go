package server

import (
	"context"
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

	// detectCache avoids repeated os.ReadDir+stat per tool call in home mode.
	DetectMu   stdsync.Mutex
	DetectDone bool
	DetectDirs []string // cached project directory names under Workdir
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
	s, cleanup := OpenServerState(cfg.Workdir, cfg.NoSync)
	defer cleanup()

	srv := NewMCPServer(s)
	stopWD := daemon.StartPPIDWatchdog(daemon.PPIDPollInterval(), func() {
		os.Exit(0)
	})
	defer stopWD()

	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
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
			s, cleanup = OpenServerState(cfg.Workdir, cfg.NoSync)
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
func OpenServerState(workdir string, noSync bool) (*Server, func()) {
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
		Database:     database,
		Orchestrator: orch,
		BgDone:       make(chan struct{}),
	}
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

	// Check for shutdown before each expensive phase so cleanup can
	// interrupt quickly rather than blocking until indexing finishes.
	select {
	case <-s.BgDone:
		return
	default:
	}

	rebuild, oldVer, err := database.NeedsRebuild()
	if err != nil {
		slog.Warn("schema revision check", "error", err)
	}
	var files, nodes int
	if rebuild {
		slog.Info("full rebuild", "from", oldVer, "to", db.SchemaRevision())

		select {
		case <-s.BgDone:
			return
		default:
		}
		files, nodes, err = orch.RebuildAll()
	} else {
		slog.Info("indexing project in background...")

		select {
		case <-s.BgDone:
			return
		default:
		}
		files, nodes, err = orch.IndexAll()
		if err == nil {
			_ = database.SetSchemaRevision()
		}
	}
	if err != nil {
		slog.Warn("index warning", "error", err)
	}
	slog.Info("indexed", "files", files, "nodes", nodes, "schema", db.SchemaRevision())

	// Optional git-status assist: catch edits missed while nothing was watching.
	if dirty := sync.GitDirtySourceFiles(workdir); len(dirty) > 0 {
		select {
		case <-s.BgDone:
			return
		default:
		}
		c, n, gerr := orch.IndexChanges(dirty)
		if gerr != nil {
			slog.Warn("git-assist sync", "error", gerr)
		} else if c > 0 {
			slog.Info("git-assist sync", "files", c, "nodes", n)
		}
	}

	if noSync {
		return
	}

	select {
	case <-s.BgDone:
		return
	default:
	}
	watcher, err := sync.NewWatcher(orch, workdir)
	if err != nil {
		slog.Warn("watcher warning", "error", err)
		return
	}
	if err := watcher.Start(); err != nil {
		slog.Warn("watcher start warning", "error", err)
		return
	}
	s.Watcher.Store(watcher)
	slog.Info("auto-sync enabled")
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
