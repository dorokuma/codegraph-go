package daemon

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// SessionHandler serves one MCP client over rwc (already past the hello handshake).
// It should block until the session ends.
type SessionHandler func(ctx context.Context, rwc io.ReadWriteCloser) error

// Daemon is the shared single-writer MCP process for one project root.
type Daemon struct {
	root      string
	handler   SessionHandler
	idleAfter time.Duration

	mu         sync.Mutex
	clients    int
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
	candidates := SocketCandidates(d.root)
	if len(candidates) == 0 {
		return errNoSocketSupport
	}

	var lastErr error
	for i, path := range candidates {
		_ = os.Remove(path) // clear stale socket; we hold the lock
		ln, err := net.Listen("unix", path)
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
		if err := os.Chmod(path, 0o600); err != nil {
			log.Printf("chmod socket %s: %v", path, err)
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
	return nil
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
	defer close(d.stopped)
	log.Printf("daemon shutting down (%s; clients=%d)", reason, d.ClientCount())
	d.mu.Lock()
	if d.idleTimer != nil {
		d.idleTimer.Stop()
		d.idleTimer = nil
	}
	d.mu.Unlock()
	d.cancel()
	if d.listener != nil {
		_ = d.listener.Close()
	}
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

func (d *Daemon) acceptLoop() {
	defer d.wg.Done()
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			if d.stopping.Load() || d.ctx.Err() != nil {
				return
			}
			// Temporary accept errors: keep going.
			log.Printf("daemon accept: %v", err)
			continue
		}
		d.wg.Add(1)
		go d.serveConn(conn)
	}
}

func (d *Daemon) serveConn(conn net.Conn) {
	defer d.wg.Done()
	defer conn.Close()

	if err := WriteHello(conn, d.socketPath); err != nil {
		return
	}

	br := bufio.NewReader(conn)
	_, leftover, _ := TryReadClientHello(conn, br)

	var r io.Reader = br
	if len(leftover) > 0 {
		r = io.MultiReader(bytes.NewReader(leftover), br)
	}
	rwc := &sessionRWC{r: r, conn: conn}

	d.addClient(conn)
	defer d.dropClient(conn)

	ctx := d.ctx
	if err := d.handler(ctx, rwc); err != nil && ctx.Err() == nil {
		log.Printf("daemon session: %v", err)
	}
}

func (d *Daemon) addClient(conn net.Conn) {
	d.mu.Lock()
	d.clients++
	d.conns[conn] = struct{}{}
	if d.idleTimer != nil {
		d.idleTimer.Stop()
		d.idleTimer = nil
	}
	d.mu.Unlock()
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
	if d.clients > 0 || d.idleTimer != nil {
		return
	}
	d.idleTimer = time.AfterFunc(d.idleAfter, func() {
		d.mu.Lock()
		n := d.clients
		d.idleTimer = nil
		d.mu.Unlock()
		if n > 0 {
			d.armIdleTimer()
			return
		}
		d.Stop("idle timeout")
	})
}

func (d *Daemon) cleanupArtifacts() {
	// Only remove our lock if it still names us.
	if raw, err := os.ReadFile(d.pidPath); err == nil {
		if info := DecodeLock(raw); info != nil && info.PID == os.Getpid() {
			_ = os.Remove(d.pidPath)
		}
	}
	if d.socketPath != "" {
		_ = os.Remove(d.socketPath)
	}
	Deregister(d.root)
}

// sessionRWC presents a net.Conn (+ optional pushed reader head) as ReadWriteCloser.
type sessionRWC struct {
	r    io.Reader
	conn net.Conn
}

func (s *sessionRWC) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *sessionRWC) Write(p []byte) (int, error) { return s.conn.Write(p) }
func (s *sessionRWC) Close() error                { return s.conn.Close() }

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
				_ = os.Remove(res.PidPath)
				return err
			}
			if onReady != nil {
				if err := onReady(); err != nil {
					log.Printf("daemon onReady: %v", err)
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
