package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two clients attach to one daemon; both see the same hello pid (single writer).
func TestDaemonTwoClientsSamePID(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvIdleTimeoutMS, "2000")

	res, err := TryAcquireLock(root)
	if err != nil || res.Kind != "acquired" {
		t.Fatalf("lock: %+v err=%v", res, err)
	}
	defer os.Remove(res.PidPath)

	var sessions atomic.Int32
	handler := func(ctx context.Context, rwc io.ReadWriteCloser) error {
		sessions.Add(1)
		// Echo lines until EOF — stands in for an MCP session.
		br := bufio.NewReader(rwc)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				if _, werr := rwc.Write(line); werr != nil {
					return werr
				}
			}
			if err != nil {
				return nil
			}
		}
	}

	d := New(root, handler)
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	defer d.Stop("test end")

	// Connect two clients, verify hello pid matches daemon.
	wantPID := os.Getpid()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, br, hello, res := ConnectHello(d.SocketPath())
			if res.Outcome != "proxied" {
				errs <- fmt.Errorf("connect: %+v", res)
				return
			}
			defer conn.Close()
			if hello.PID != wantPID {
				errs <- fmt.Errorf("hello pid %d want %d", hello.PID, wantPID)
				return
			}
			if hello.Codegraph != PackageVersion {
				errs <- fmt.Errorf("version %q", hello.Codegraph)
				return
			}
			_ = WriteClientHello(conn)
			// Send a ping line and expect echo.
			if _, err := conn.Write([]byte("ping\n")); err != nil {
				errs <- err
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			line, err := br.ReadString('\n')
			if err != nil {
				errs <- err
				return
			}
			if line != "ping\n" {
				errs <- fmt.Errorf("echo %q", line)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if sessions.Load() != 2 {
		t.Fatalf("sessions=%d want 2", sessions.Load())
	}
}

// Competing lock holders: only one acquires; the other sees taken.
func TestTwoWritersLockArbitration(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(CodeGraphDir(root), 0o755)

	a, err := TryAcquireLock(root)
	if err != nil || a.Kind != "acquired" {
		t.Fatalf("a: %+v %v", a, err)
	}
	defer os.Remove(a.PidPath)

	// Simulate second process by just calling again (same pid is fine for "taken").
	b, err := TryAcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != "taken" {
		t.Fatalf("second writer got %q — would double-write", b.Kind)
	}
}

func TestDaemonIdleExit(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(CodeGraphDir(root), 0o755)
	t.Setenv(EnvIdleTimeoutMS, "150")

	res, err := TryAcquireLock(root)
	if err != nil || res.Kind != "acquired" {
		t.Fatal(res, err)
	}
	// Don't remove pidpath ourselves — idle stop should clean it.

	handler := func(ctx context.Context, rwc io.ReadWriteCloser) error {
		<-ctx.Done()
		return nil
	}
	d := New(root, handler)
	// Rebuild idle from env after Setenv
	d.idleAfter = IdleTimeout()
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		d.Wait()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(3 * time.Second):
		d.Stop("test timeout")
		t.Fatal("daemon did not idle-exit")
	}

	// Lockfile should be gone after idle cleanup.
	if _, err := os.Stat(PidPath(root)); !os.IsNotExist(err) {
		t.Fatalf("lockfile still present: %v", err)
	}
	// Socket gone too
	if d.SocketPath() != "" {
		if _, err := os.Stat(d.SocketPath()); !os.IsNotExist(err) {
			t.Fatalf("socket still present")
		}
	}
}

// TestRunAsDaemonOnReadyFailureStopsAndReleasesLock (L4): a failing onReady
// (the daemon-mode equivalent of "DB cannot be opened") must END the daemon
// lifecycle instead of keeping a DB-less zombie alive. Afterwards the pidfile
// (daemon lock) and the socket must be gone, the lock must be re-acquirable
// (clients can fall back to direct / spawn a fresh daemon), and the error
// must reach the RunAsDaemon caller so the process exits non-zero.
func TestRunAsDaemonOnReadyFailureStopsAndReleasesLock(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	onReadyErr := errors.New("open database: injected failure")

	handler := func(ctx context.Context, rwc io.ReadWriteCloser) error { return nil }
	err := RunAsDaemon(root, handler, func() error { return onReadyErr })
	if err == nil {
		t.Fatal("RunAsDaemon must return the onReady error")
	}
	if !errors.Is(err, onReadyErr) {
		t.Fatalf("returned error %v does not wrap onReady error", err)
	}
	// Lock released: pidfile removed by Stop's cleanupArtifacts.
	if _, serr := os.Stat(PidPath(root)); !os.IsNotExist(serr) {
		t.Fatalf("pidfile still present after onReady failure: %v", serr)
	}
	// Socket removed: no client can attach to a DB-less daemon. Check every
	// candidate path — Start binds the first bindable candidate (preferred
	// in-project path, or the tmpdir fallback when that path is too long /
	// unusable, see SocketCandidates), so the preferred path alone could miss
	// the socket actually bound.
	for _, sock := range SocketCandidates(root) {
		if _, serr := os.Stat(sock); !os.IsNotExist(serr) {
			t.Fatalf("socket still present after onReady failure: %s: %v", sock, serr)
		}
	}
	// The lock is re-acquirable — direct-mode fallback / fresh spawn works.
	res, aerr := TryAcquireLock(root)
	if aerr != nil || res.Kind != "acquired" {
		t.Fatalf("lock not re-acquirable after onReady failure: %+v err=%v", res, aerr)
	}
	_ = os.Remove(res.PidPath)
}

func TestHelloVersionMismatch(t *testing.T) {
	// Stand up a fake listener that speaks a wrong version.
	dir := t.TempDir()
	sock := filepath.Join(dir, "t.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte(`{"codegraph":"9.9.9","pid":1,"socketPath":"x","protocol":1}` + "\n"))
		time.Sleep(200 * time.Millisecond)
	}()

	_, _, _, res := ConnectHello(sock)
	if res.Outcome != "fallback-needed" {
		t.Fatalf("want fallback on mismatch, got %+v", res)
	}
}

// TestDaemonExitsWhenWorkdirRemoved is the regression for the stale narrow
// daemon: a session narrowed into a .wt/<repo>/<branch> worktree keeps
// serving that path, and when the dispatched agent deletes the worktree the
// daemon used to sit there holding the DB flock, the pidfile and the socket
// until the (five-minute) idle timeout — wasting the lock and blocking a
// fresh spawn under a recreated path. The workdir liveness watch must end it
// at the next tick, but never in the middle of a live session.
func TestDaemonExitsWhenWorkdirRemoved(t *testing.T) {
	oldInterval := rootLivenessInterval
	rootLivenessInterval = 20 * time.Millisecond
	t.Cleanup(func() { rootLivenessInterval = oldInterval })

	start := func(root string) *Daemon {
		t.Helper()
		if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
			t.Fatal(err)
		}
		// Idle exit must not be what ends this daemon — long timeout, so only
		// the workdir watch can fire.
		t.Setenv(EnvIdleTimeoutMS, "3600000")
		handler := func(ctx context.Context, rwc io.ReadWriteCloser) error {
			<-ctx.Done()
			return nil
		}
		d := New(root, handler)
		d.idleAfter = IdleTimeout()
		if err := d.Start(); err != nil {
			t.Fatal(err)
		}
		return d
	}
	exitedWithin := func(d *Daemon, within time.Duration) bool {
		t.Helper()
		done := make(chan struct{})
		go func() { d.Wait(); close(done) }()
		select {
		case <-done:
			return true
		case <-time.After(within):
			return false
		}
	}

	// (a) workdir removed, no session → the daemon stops by itself.
	root := t.TempDir()
	d := start(root)
	select {
	case <-d.ctx.Done():
		d.Stop("test cleanup")
		t.Fatal("daemon exited while its workdir still existed")
	case <-time.After(200 * time.Millisecond): // several ticks, still alive
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if !exitedWithin(d, 5*time.Second) {
		d.Stop("test cleanup")
		t.Fatal("daemon did not exit after its workdir was removed")
	}
	if !d.stopping.Load() {
		t.Fatal("daemon must be marked stopping once the workdir is gone")
	}

	// (b) the same, but with a live session at the moment the workdir goes:
	// the session is never cut, the daemon leaves when the session does.
	root2 := t.TempDir()
	d2 := start(root2)
	client, served := net.Pipe()
	defer client.Close()
	d2.addClient(served)
	if err := os.RemoveAll(root2); err != nil {
		t.Fatal(err)
	}
	if exitedWithin(d2, 300*time.Millisecond) {
		served.Close()
		t.Fatal("daemon cut an in-flight session when its workdir vanished")
	}
	// The session ends the normal way (serveConn's defer): only then is the
	// daemon allowed to leave.
	d2.dropClient(served)
	served.Close()
	if !exitedWithin(d2, 5*time.Second) {
		d2.Stop("test cleanup")
		t.Fatal("daemon did not exit after the last session ended and the workdir was gone")
	}
}

// TestDaemonStopIfIdleDoesNotCutInFlightOrHandshakingSession is the
// concurrency regression for the accept→register race: a connection that
// has been Accept()ed but has not completed ClientHello (the handshake
// window before addClient) used to be invisible to ClientCount, so a
// "clients==0" idle/workdir stop could cut the listener out from under
// it. With the in-flight counter, stopIfIdle must refuse while the
// handshake is parked, must refuse while a session is live, and must
// succeed cleanly once everything has drained.
func TestDaemonStopIfIdleDoesNotCutInFlightOrHandshakingSession(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	// Idle exit must not be what ends this daemon — long timeout, so only
	// explicit stopIfIdle calls decide.
	t.Setenv(EnvIdleTimeoutMS, "3600000")

	var sessions atomic.Int32
	sessionStarted := make(chan struct{}, 8)
	sessionRelease := make(chan struct{})
	handler := func(ctx context.Context, rwc io.ReadWriteCloser) error {
		sessions.Add(1)
		sessionStarted <- struct{}{}
		select {
		case <-sessionRelease:
		case <-ctx.Done():
		}
		return nil
	}

	d := New(root, handler)
	d.idleAfter = IdleTimeout()
	res, err := TryAcquireLock(root)
	if err != nil || res.Kind != "acquired" {
		t.Fatalf("lock: %+v err=%v", res, err)
	}
	defer os.Remove(res.PidPath)
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}

	// --- Phase 1: a connection parked INSIDE the handshake window ---
	// (accepted by the kernel backlog / serveConn between Accept and
	// addClient). It never sends ClientHello, so it stays in-flight
	// without ever registering as a client. To make the window
	// deterministic we check the internal counter rather than racing
	// Accept scheduling.
	raw, err := net.Dial("unix", d.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Read the server hello so we know serveConn is running and the conn
	// has been counted in-flight.
	br := bufio.NewReader(raw)
	if _, err := br.ReadBytes('\n'); err != nil {
		t.Fatalf("server hello: %v", err)
	}
	// Spin briefly until the accept loop's counter lands (the hello was
	// written AFTER inFlight++, but give the scheduler a beat anyway).
	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		n := d.inFlight
		d.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accepted connection never entered the in-flight window")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The connection is in-flight but NOT a client: stopIfIdle must
	// refuse instead of cutting it.
	if d.stopIfIdle("test: should refuse") {
		t.Fatal("stopIfIdle cut a connection still in the handshake window")
	}
	if d.stopping.Load() {
		t.Fatal("daemon must not be marked stopping while a conn is in-flight")
	}
	// The handshake completes now: the parked conn sends ClientHello and
	// becomes a real session.
	if err := WriteClientHello(raw); err != nil {
		t.Fatal(err)
	}
	<-sessionStarted
	// Now registered: stopIfIdle still refuses (a live session).
	if d.stopIfIdle("test: should refuse") {
		t.Fatal("stopIfIdle cut a live session")
	}
	// --- Phase 2: a second concurrent connection also survives ---
	raw2, err := net.Dial("unix", d.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw2.Close()
	br2 := bufio.NewReader(raw2)
	if _, err := br2.ReadBytes('\n'); err != nil {
		t.Fatalf("server hello (conn2): %v", err)
	}
	if err := WriteClientHello(raw2); err != nil {
		t.Fatal(err)
	}
	<-sessionStarted
	if d.stopIfIdle("test: should refuse") {
		t.Fatal("stopIfIdle cut concurrent live sessions")
	}
	if sessions.Load() != 2 {
		t.Fatalf("sessions=%d want 2", sessions.Load())
	}

	// --- Phase 3: sessions end; the NEXT stopIfIdle wins cleanly ---
	close(sessionRelease)
	raw.Close()
	raw2.Close()
	deadline = time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		idle := d.clients == 0 && d.inFlight == 0
		d.mu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sessions never drained")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !d.stopIfIdle("test: drained") {
		t.Fatal("stopIfIdle refused after all sessions drained")
	}
	if !d.stopping.Load() {
		t.Fatal("daemon must be marked stopping after a successful stopIfIdle")
	}
	// Wait completes (stopIfIdle ran the full shutdown: listener closed,
	// conns drained, artifacts removed).
	done := make(chan struct{})
	go func() { d.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not finish shutdown after stopIfIdle")
	}
	// A fresh dial must fail — the listener is gone, not half-stopped.
	if c, err := net.Dial("unix", d.SocketPath()); err == nil {
		c.Close()
		t.Fatal("listener still accepting after stopIfIdle")
	}
	// And a duplicate Stop is a no-op, not a double-close panic.
	d.Stop("test cleanup")
}

// TestApplyProcessLimitsMaxProcs: GOMAXPROCS capping honours the env-var
// precedence rule — an explicit GOMAXPROCS always wins over max_procs.
func TestApplyProcessLimitsMaxProcs(t *testing.T) {
	prev := runtime.GOMAXPROCS(-1)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	ApplyProcessLimits(0, 1)
	if got := runtime.GOMAXPROCS(-1); got != 1 {
		t.Fatalf("GOMAXPROCS = %d, want 1 after ApplyProcessLimits(0,1)", got)
	}

	t.Setenv("GOMAXPROCS", "7")
	ApplyProcessLimits(0, 2)
	if got := runtime.GOMAXPROCS(-1); got != 1 {
		t.Fatalf("explicit GOMAXPROCS env must not be overridden, got %d", got)
	}

	// nice=0 / maxProcs=0 are pure no-ops (also exercises the non-error
	// path of a setpriority that may fail without CAP_SYS_NICE).
	// Non-zero nice levels are verified only in limits_linux_test.go's
	// re-executed child process — calling ApplyProcessLimits(19, 0) here
	// would renice the go test process itself, and a non-root user
	// running -count=3 could no longer raise the priority back in later
	// rounds (nice can only be lowered without CAP_SYS_NICE), making the
	// isolated child's nice=12 setpriority fail.
	ApplyProcessLimits(0, 0)
}
