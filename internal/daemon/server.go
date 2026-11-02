package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dorokuma/codegraph-go/internal/cgdir"
)

// SessionHandler serves one MCP client over rwc (already past the hello handshake).
// It should block until the session ends.
type SessionHandler func(ctx context.Context, rwc io.ReadWriteCloser) error

// Daemon is the shared single-writer MCP process for one project root.
type Daemon struct {
	root      string
	handler   SessionHandler
	idleAfter time.Duration

	mu      sync.Mutex
	clients int
	// inFlight counts accepted connections that have not completed
	// registration yet (serveConn between Accept and addClient). It is
	// what makes stopIfIdle atomic: a connection parked in the handshake
	// window is still "someone being served", so the daemon must not
	// exit underneath it. Guarded by mu like clients.
	inFlight   int
	conns      map[net.Conn]struct{}
	idleTimer  *time.Timer
	stopping   atomic.Bool
	listener   net.Listener
	socketPath string
	pidPath    string
	cancel     context.CancelFunc
	ctx        context.Context
	wg         sync.WaitGroup
	stopped    chan struct{} // closed after Stop finishes cleanup
	// dirID is the (dev, ino) fingerprint of the .codegraph directory,
	// recorded by Start right after cgdir.Ensure. Every path-based removal
	// (stale socket before bind, pidfile/socket in cleanupArtifacts) must
	// re-verify it first: if the directory behind .codegraph was swapped
	// while the daemon ran, the removals skip instead of deleting through
	// attacker-chosen paths. nil until Start records it (and then: fail
	// safe — removals under .codegraph are refused).
	dirID *dirIdentity
}

// New constructs a Daemon. Call Start after TryAcquireLock succeeded.
func New(root string, handler SessionHandler) *Daemon {
	ctx, cancel := context.WithCancel(context.Background())
	return &Daemon{
		root:      root,
		handler:   handler,
		idleAfter: IdleTimeout(),
		pidPath:   PidPath(root),
		ctx:       ctx,
		cancel:    cancel,
		conns:     make(map[net.Conn]struct{}),
		stopped:   make(chan struct{}),
	}
}

// Start binds the first usable socket candidate and accepts connections.
// Resolves once listening. Blocks in Accept loop on a background goroutine;
// the process stays alive via the listener + client sessions + idle timer.
func (d *Daemon) Start() error {
	// Guard .codegraph before the bind creates an inode in it: bind(2)
	// mknods the socket, and the os.Remove of a stale socket below would
	// delete through a symlink. Same jail line as db.Open, which runs later
	// in onReady — this covers the write points that happen before it. The
	// returned identity comes from the stat Ensure itself runs right after
	// its verification, so jail check and fingerprint are one step with no
	// second stat in between for a swap to slip into.
	cgID, err := cgdir.Ensure(CodeGraphDir(d.root))
	if err != nil {
		return err
	}
	// Record the directory identity Ensure just verified. From here on,
	// every path-based deletion verifies against this fingerprint (the
	// stale-socket remove in the bind loop below, and cleanupArtifacts at
	// shutdown) — the removal-side counterpart of db.Open's recheckDir.
	dirID := &dirIdentity{dev: cgID.Dev, ino: cgID.Ino}
	d.dirID = dirID
	log.Printf("daemon start: .codegraph dir identity dev=%d ino=%d (also recorded in the pidfile)", dirID.dev, dirID.ino)
	candidates := SocketCandidates(d.root)
	if len(candidates) == 0 {
		return errNoSocketSupport
	}

	var lastErr error
	for i, path := range candidates {
		d.removeArtifact(path) // clear stale socket; we hold the lock
		ln, err := listenUnixWithUmask(path)
		if err != nil {
			lastErr = err
			// EADDRINUSE → don't relocate (another live binder); other errors try next.
			if isAddrInUse(err) {
				break
			}
			if i+1 < len(candidates) {
				log.Printf("socket %s unusable (%v); relocating to %s", path, err, candidates[i+1])
			}
			continue
		}
		// The runtime unlinks a Unix socket file through its PATH when the
		// listener closes (net.UnixListener unlink-on-close) — a removal the
		// dir-identity guard below cannot see. Disable it so the guarded
		// removeArtifact calls are the ONLY socket removals: on the normal
		// path they delete the just-bound socket exactly as before, and
		// after a .codegraph swap they skip instead of deleting through the
		// attacker's path.
		if u, ok := ln.(*net.UnixListener); ok {
			u.SetUnlinkOnClose(false)
		}
		if err := chmodSocket(path, 0o600); err != nil {
			// Audit medium: a socket left with broader permissions than 0600
			// would let other users connect to a full-privilege MCP endpoint
			// (read source, write facts). Failing the bind is safer than
			// accepting on a mis-permissioned socket — the caller exits
			// non-zero instead of continuing to Accept.
			_ = ln.Close()
			d.removeArtifact(path)
			return fmt.Errorf("chmod socket %s: %w", path, err)
		}
		d.listener = ln
		d.socketPath = path
		break
	}
	if d.listener == nil {
		return lastErr
	}

	lock := LockInfo{
		PID:        os.Getpid(),
		Version:    PackageVersion,
		SocketPath: d.socketPath,
		StartedAt:  time.Now().UnixMilli(),
		ProcStart:  procStartTime(os.Getpid()),
	}
	// Rewrite pidfile if we relocated off candidate 0.
	if pref := PreferredSocket(d.root); pref != "" && d.socketPath != pref {
		if err := RewriteLock(d.pidPath, lock); err != nil {
			log.Printf("rewrite lock %s: %v", d.pidPath, err)
		}
	}

	Register(Record{
		Root:       d.root,
		PID:        lock.PID,
		Version:    lock.Version,
		SocketPath: lock.SocketPath,
		StartedAt:  lock.StartedAt,
	})

	log.Printf("daemon listening on %s (pid %d, v%s). idle timeout %s",
		d.socketPath, os.Getpid(), PackageVersion, d.idleAfter)

	d.armIdleTimer()

	d.wg.Add(1)
	go d.acceptLoop()
	// Workdir liveness: a workdir can disappear underneath a running daemon
	// (dispatched agents delete their .wt/<repo>/<branch> worktree while a
	// narrowed daemon still serves it). Without this the daemon keeps the DB
	// lock, the pidfile and the socket until the idle timeout fires — five
	// minutes of dead weight that also blocks a fresh spawn under the same
	// path. Poll cheaply and stop as soon as nobody is served any more.
	//
	// Deliberately NOT tracked by d.wg: this goroutine calls Stop, and Stop
	// waits for d.wg — joining the watchdog into its own wait group would
	// deadlock the shutdown it is trying to start.
	go d.watchRootLiveness()
	return nil
}

// rootLivenessInterval is how often a running daemon checks that its workdir
// still exists. A var so tests shrink it; 0 disables the watchdog.
var rootLivenessInterval = 15 * time.Second

// watchRootLiveness stops the daemon when its workdir has been removed while
// no session is active, so a daemon narrowed into a deleted git worktree
// releases the DB lock, the pidfile and the socket immediately instead of
// idling out minutes later.
//
// Active sessions are never interrupted: a client is mid-request, the worst
// outcome would be an error in the middle of a tool call. The next idle
// window (or the next tick with no clients) ends the process. Stop runs
// synchronously here and the goroutine returns right after it — which is why
// Start must not join this goroutine into d.wg (Stop waits for d.wg).
func (d *Daemon) watchRootLiveness() {
	interval := rootLivenessInterval
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			if d.root == "" || d.stopping.Load() {
				continue
			}
			if _, err := os.Stat(d.root); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				// EACCES / EIO / ELOOP and friends do not prove the workdir is
				// gone — never tear a serving daemon down over a stat error we
				// cannot attribute to a deleted directory.
				continue
			}
			// Atomic idle stop: a connection accepted-but-not-registered
			// still counts (inFlight), so the check-and-stop cannot race a
			// handshake the way a ClientCount-then-Stop pair could.
			if d.stopIfIdle("workdir removed") {
				log.Printf("daemon workdir %s no longer exists; shut down to release the lock and sockets", d.root)
				return
			}
		}
	}
}

// Wait blocks until Stop has finished (idle/signal/explicit) and sessions drain.
func (d *Daemon) Wait() {
	<-d.stopped
}

// SocketPath returns the bound path (valid after Start).
func (d *Daemon) SocketPath() string { return d.socketPath }

// ClientCount is the live session count.
func (d *Daemon) ClientCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.clients
}

// Stop gracefully shuts down.
func (d *Daemon) Stop(reason string) {
	if !d.stopping.CompareAndSwap(false, true) {
		return
	}
	// Same beginShutdown + listener close + finishShutdown sequence as
	// stopIfIdle; the listener is closed outside the mutex so a pending
	// Accept error path never waits on d.mu while holding it.
	ln := d.beginShutdown()
	log.Printf("daemon shutting down (%s; clients=%d)", reason, d.ClientCount())
	if ln != nil {
		_ = ln.Close()
	}
	d.finishShutdown()
}

// stopIfIdle is the atomic counterpart of Stop used by the autonomous
// shutdown paths (idle timer, workdir watchdog): under a single d.mu hold
// it checks that no session is registered AND no accepted connection is
// still in the handshake window, flips d.stopping, then closes the
// listener and runs the shared shutdown — so no Accept can slip between
// the count check and the stop. Returns false (nothing stopped) when the
// daemon is busy or already stopping.
func (d *Daemon) stopIfIdle(reason string) bool {
	d.mu.Lock()
	if d.clients > 0 || d.inFlight > 0 {
		d.mu.Unlock()
		return false
	}
	// Flip stopping while still holding mu: a connection that completes
	// its handshake right now sees the flag in addClient and is refused
	// instead of registering into a daemon that is already draining.
	if !d.stopping.CompareAndSwap(false, true) {
		d.mu.Unlock()
		return false
	}
	ln := d.beginShutdownLocked()
	d.mu.Unlock()
	log.Printf("daemon shutting down (%s; clients=0)", reason)
	if ln != nil {
		_ = ln.Close()
	}
	d.finishShutdown()
	return true
}

// beginShutdown detaches the listener + idle timer under d.mu (the
// caller owns the stopping flag via CompareAndSwap). It returns the
// listener so the caller can close it AFTER releasing the mutex —
// closing wakes a blocked Accept, so keeping it outside the critical
// section keeps the lock hold minimal.
func (d *Daemon) beginShutdown() net.Listener {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.beginShutdownLocked()
}

// beginShutdownLocked is beginShutdown with d.mu already held.
func (d *Daemon) beginShutdownLocked() net.Listener {
	if d.idleTimer != nil {
		d.idleTimer.Stop()
		d.idleTimer = nil
	}
	return d.listener
}

// finishShutdown is the shared tail of Stop and stopIfIdle: cancel the
// context, force-close live sessions, wait for accept loop + sessions to
// drain, then remove lock/socket artifacts. d.stopped is closed exactly
// once — both entry points funnel through stopping.CompareAndSwap.
func (d *Daemon) finishShutdown() {
	defer close(d.stopped)
	d.cancel()
	// Force-close live clients so session handlers unblock.
	d.mu.Lock()
	for c := range d.conns {
		_ = c.Close()
	}
	d.mu.Unlock()
	// Wait for accept loop + in-flight sessions before removing lock/socket.
	d.wg.Wait()
	d.cleanupArtifacts()
}

// isTemporaryAcceptError reports whether an accept error is transient and
// the accept loop should keep serving: a timeout, or an underlying errno the
// runtime classes as temporary (EINTR, EMFILE, ENFILE, EAGAIN/ETIMEDOUT).
// It replaces the deprecated net.Error.Temporary with equivalent semantics
// for accept errors.
func isTemporaryAcceptError(err error) bool {
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		return false
	}
	if opErr.Timeout() {
		return true
	}
	var errno syscall.Errno
	if errors.As(opErr.Err, &errno) {
		return errno.Temporary()
	}
	return false
}

func (d *Daemon) acceptLoop() {
	defer d.wg.Done()
	var permErrs int
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			if d.stopping.Load() || d.ctx.Err() != nil {
				return
			}
			if isTemporaryAcceptError(err) {
				// Temporary accept errors: keep going.
				log.Printf("daemon accept (temporary): %v", err)
				permErrs = 0
				continue
			}
			// Permanent error — back off with a cap and count.
			permErrs++
			log.Printf("daemon accept (permanent #%d): %v", permErrs, err)
			if permErrs >= 5 {
				log.Printf("daemon accept: %d consecutive permanent errors, shutting down", permErrs)
				d.Stop("accept-permanent-failures")
				return
			}
			// Exponential backoff: 100ms, 200ms, 400ms, 800ms, 1s cap.
			sleep := 100 * time.Millisecond
			for i := 1; i < permErrs && sleep < time.Second; i++ {
				sleep *= 2
			}
			time.Sleep(sleep)
			continue
		}
		permErrs = 0
		// Register the accepted connection under the same lock that
		// stopIfIdle inspects: either the daemon is already stopping (drop
		// the conn) or the conn is counted in-flight BEFORE serveConn
		// starts, so the idle/stop check can never miss it.
		d.mu.Lock()
		if d.stopping.Load() {
			d.mu.Unlock()
			_ = conn.Close()
			return
		}
		d.inFlight++
		if d.idleTimer != nil {
			d.idleTimer.Stop()
			d.idleTimer = nil
		}
		d.mu.Unlock()
		d.wg.Add(1)
		go d.serveConn(conn)
	}
}

func (d *Daemon) serveConn(conn net.Conn) {
	defer d.wg.Done()
	defer conn.Close()
	// The accept loop counted this connection in-flight before spawning
	// us; decrement on the way out, whatever the handshake did.
	defer d.finishConn()
	// Audit critical: an unrecovered panic in a session goroutine exits the
	// whole process — every other session's MCP connection dies with it and
	// the index writer disappears. Recover here so a panicking session (or
	// transport hiccup) costs only that connection; the tool-dispatch layer
	// (server.toolCodegraph) additionally converts per-tool panics into
	// single-call errors so the session itself survives.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("daemon session panic (pid %d): %v\n%s", os.Getpid(), r, debug.Stack())
		}
	}()

	if err := WriteHello(conn, d.socketPath); err != nil {
		return
	}

	br := bufio.NewReader(conn)
	chlo, leftover, ok := TryReadClientHello(conn, br)

	// Optional socket token auth (audit high): when CODEGRAPH_MCP_TOKEN is
	// set on the daemon, a session must present the matching token in its
	// client hello. A session that never sent a hello (ok=false) cannot be
	// authenticated and is dropped. The token itself never appears in any
	// log line.
	if tok := MCPToken(); tok != "" {
		if !ok || !constantTimeTokenEqual(chlo.Token, tok) {
			log.Printf("daemon: rejected connection on %s: missing or invalid socket token (pid %d)", d.socketPath, os.Getpid())
			return
		}
	}

	// Bound concurrent sessions: a local flood of connections must not
	// exhaust fds/goroutines, and a pile of hung sessions must not block
	// idle exit forever (audit medium). Reject beyond the cap before the
	// session is counted.
	if d.ClientCount() >= maxSessions {
		log.Printf("daemon: rejected connection on %s: %d sessions active (max %d)", d.socketPath, d.ClientCount(), maxSessions)
		return
	}

	var r io.Reader = br
	if len(leftover) > 0 {
		r = io.MultiReader(bytes.NewReader(leftover), br)
	}
	rwc := &sessionRWC{r: r, conn: conn, idle: sessionIdleTimeout}

	if !d.addClient(conn) {
		// The daemon began stopping between Accept and registration —
		// never start a handler for a session the shutdown already owns.
		return
	}
	defer d.dropClient(conn)

	ctx := d.ctx
	if err := d.handler(ctx, rwc); err != nil && ctx.Err() == nil {
		log.Printf("daemon session: %v", err)
	}
}

// addClient registers a live session. It returns false when the daemon
// is already stopping: registration and the stopping flag are checked
// under the same mutex, so a session can never slip past beginShutdown
// and get stranded uncounted.
func (d *Daemon) addClient(conn net.Conn) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping.Load() {
		return false
	}
	d.clients++
	d.conns[conn] = struct{}{}
	if d.idleTimer != nil {
		d.idleTimer.Stop()
		d.idleTimer = nil
	}
	return true
}

// finishConn is the accept-loop counterpart of addClient/dropClient: it
// decrements the in-flight count serveConn was spawned with, and
// rearms the idle timer when the last connection — registered or
// in-flight — is gone.
func (d *Daemon) finishConn() {
	d.mu.Lock()
	if d.inFlight > 0 {
		d.inFlight--
	}
	idle := d.clients == 0 && d.inFlight == 0
	d.mu.Unlock()
	if idle {
		d.armIdleTimer()
	}
}

func (d *Daemon) dropClient(conn net.Conn) {
	d.mu.Lock()
	delete(d.conns, conn)
	if d.clients > 0 {
		d.clients--
	}
	n := d.clients
	d.mu.Unlock()
	if n == 0 {
		d.armIdleTimer()
	}
}

func (d *Daemon) armIdleTimer() {
	if d.idleAfter <= 0 || d.stopping.Load() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.clients > 0 || d.inFlight > 0 || d.idleTimer != nil {
		return
	}
	d.idleTimer = time.AfterFunc(d.idleAfter, func() {
		// Atomic idle stop: a busy daemon (session live or handshake in
		// flight) refuses and simply rearms for the next window.
		if !d.stopIfIdle("idle timeout") {
			d.armIdleTimer()
		}
	})
}

func (d *Daemon) cleanupArtifacts() {
	// Only remove our lock if it still names us.
	if raw, err := os.ReadFile(d.pidPath); err == nil {
		if info := DecodeLock(raw); info != nil && info.PID == os.Getpid() {
			d.removeArtifact(d.pidPath)
		}
	}
	if d.socketPath != "" {
		d.removeArtifact(d.socketPath)
	}
	Deregister(d.root)
}

// dirIDVerified reports whether path-based removals may proceed: the
// .codegraph path must still resolve to the directory identity Start
// recorded. dirID == nil (Start never ran) means unverifiable — fail safe
// by refusing.
func (d *Daemon) dirIDVerified() bool {
	return d.dirID != nil && d.dirID.matches(CodeGraphDir(d.root))
}

// removeArtifact removes a daemon-created file, guarded against the
// red-team swap: when the path sits under .codegraph and the directory
// behind the path no longer matches the identity recorded at Start, the
// removal is skipped with a log line instead of deleting through the
// swapped path (a running daemon SIGTERMed after .codegraph became a
// symlink would otherwise remove attacker-planted daemon.pid/daemon.sock
// in the symlink target). Paths outside .codegraph (the tmpdir socket
// fallback) are not governed by that identity and are removed
// unconditionally, as before. Artifacts deliberately left behind by a skip
// are handled by the next start (TryAcquireLock / ClearStaleLock /
// stale-socket clearing).
func (d *Daemon) removeArtifact(path string) {
	if filepath.Dir(path) == CodeGraphDir(d.root) && !d.dirIDVerified() {
		if d.dirID == nil {
			log.Printf("cannot verify .codegraph directory; skipping artifact removal (%s)", path)
		} else {
			log.Printf("directory changed since open; skipping artifact removal (%s)", path)
		}
		return
	}
	_ = os.Remove(path)
}

// chmodSocket applies the socket permission mask. A var so tests can inject
// failures (audit medium: a chmod failure must abort Start, never accept on
// an over-permissive socket).
var chmodSocket = os.Chmod

// maxSessions caps concurrent MCP sessions per daemon. Generous for real
// use (one client per host agent); bounds fd/goroutine growth from a
// connection flood and keeps idle-exit reachable (audit medium: no max
// connection count before).
const maxSessions = 64

// sessionIdleTimeout is the per-session read/write idle deadline, refreshed
// on every read/write. A client that goes silent (hung connection, no
// traffic) is disconnected after this, dropping the session count so the
// daemon's idle timer can eventually stop the process. Generous enough that
// a legitimately idle MCP session is never cut: real sessions exchange
// requests whenever the agent works.
const sessionIdleTimeout = 30 * time.Minute

// sessionRWC presents a net.Conn (+ optional pushed reader head) as ReadWriteCloser.
type sessionRWC struct {
	r    io.Reader
	conn net.Conn
	idle time.Duration
}

func (s *sessionRWC) Read(p []byte) (int, error) {
	if s.idle > 0 {
		_ = s.conn.SetReadDeadline(time.Now().Add(s.idle))
	}
	return s.r.Read(p)
}
func (s *sessionRWC) Write(p []byte) (int, error) {
	if s.idle > 0 {
		_ = s.conn.SetWriteDeadline(time.Now().Add(s.idle))
	}
	return s.conn.Write(p)
}
func (s *sessionRWC) Close() error { return s.conn.Close() }

// RunAsDaemon is the CODEGRAPH_DAEMON_INTERNAL entry: acquire lock, start, wait.
// onReady is called once the socket is listening (index/watcher start here).
// handler serves each MCP session.
func RunAsDaemon(root string, handler SessionHandler, onReady func() error) error {
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		res, err := TryAcquireLock(root)
		if err != nil {
			return err
		}
		if res.Kind == "acquired" {
			d := New(root, handler)
			if err := d.Start(); err != nil {
				// Same removal discipline as cleanupArtifacts: go through the
				// dir-identity guard instead of deleting through a bare path.
				// When Start failed before the identity was recorded (the
				// cgdir.Ensure identity step), the guard refuses and
				// the pidfile stays behind — it names this (now exiting)
				// process, so the next start's ClearStaleLock removes it.
				d.removeArtifact(res.PidPath)
				return err
			}
			if onReady != nil {
				if err := onReady(); err != nil {
					// L4: onReady failure (typically the DB cannot be opened)
					// must END the daemon, not just log and keep waiting: a
					// DB-less daemon holding the pidfile lock + socket would
					// be a zombie that clients attach to yet can never serve,
					// and it blocks both fresh spawns and direct-mode
					// fallback. Stop releases the socket and removes our
					// pidfile (cleanupArtifacts), then the error propagates to
					// the caller so the process exits non-zero — "DB cannot be
					// opened = startup failure" stays true, just via error
					// return instead of os.Exit.
					log.Printf("daemon onReady: %v; shutting down", err)
					d.Stop("onReady failed")
					return err
				}
			}
			// Signal handlers
			go watchSignals(func() { d.Stop("signal") })
			d.Wait()
			return nil
		}
		// Taken.
		existing := res.Existing
		if existing != nil && existing.PID > 0 && IsProcessAlive(existing.PID) {
			// Clean exit path (B3): we never acquired the lock and hold no
			// socket/goroutines, so nothing needs cleanup — a plain return
			// (not os.Exit) lets caller defers run.
			log.Printf("another daemon (pid %d) holds the lock; exiting", existing.PID)
			return nil
		}
		ClearStaleLock(res.PidPath, 0)
		if existing != nil {
			ClearStaleLock(res.PidPath, existing.PID)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errLockGiveUp
}

// --- tiny error helpers ---

type simpleError string

func (e simpleError) Error() string { return string(e) }

const (
	errNoSocketSupport simpleError = "daemon sockets not supported on this platform"
	errLockGiveUp      simpleError = "could not acquire daemon lock"
)

func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	// net.OpError → syscall.EADDRINUSE
	msg := err.Error()
	return contains(msg, "address already in use") || contains(msg, "EADDRINUSE")
}
