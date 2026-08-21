package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitConnClosed polls conn until the daemon closes it (or fails the test).
// A live-but-silent session makes Read return a deadline error, which is
// treated as "still open"; only EOF/close counts as closed.
func waitConnClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	buf := make([]byte, 1)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := conn.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			return // closed (EOF / reset) — what we expect
		}
		// Session still alive and talking; keep polling.
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("connection was not closed by the daemon")
}

// TestDaemonSessionPanicDoesNotKillDaemon (audit critical): a panicking
// session handler must cost only that connection — the daemon process (and
// every other session) survives. Before the recover, the panic escaped to
// os.Exit and took the whole shared daemon down.
func TestDaemonSessionPanicDoesNotKillDaemon(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := TryAcquireLock(root)
	if err != nil || res.Kind != "acquired" {
		t.Fatalf("lock: %+v err=%v", res, err)
	}
	defer os.Remove(res.PidPath)

	var sessions atomic.Int32
	handler := func(ctx context.Context, rwc io.ReadWriteCloser) error {
		if sessions.Add(1) == 1 {
			panic("boom: first session panics")
		}
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

	// Client 1: its session panics; the connection must be closed, the
	// daemon must stay up.
	c1, _, _, pr := ConnectHello(d.SocketPath())
	if pr.Outcome != "proxied" {
		t.Fatalf("connect 1: %+v", pr)
	}
	if err := WriteClientHello(c1); err != nil {
		t.Fatal(err)
	}
	_, _ = c1.Write([]byte("ping\n"))
	waitConnClosed(t, c1)

	// Client 2: fully served, proving the daemon still works.
	c2, br2, _, pr := ConnectHello(d.SocketPath())
	if pr.Outcome != "proxied" {
		t.Fatalf("connect 2: %+v", pr)
	}
	defer c2.Close()
	if err := WriteClientHello(c2); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := br2.ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo after panic: line=%q err=%v", line, err)
	}

	if got := sessions.Load(); got != 2 {
		t.Fatalf("sessions=%d, want 2 (daemon must survive the panic)", got)
	}
}

// TestDaemonTokenAuth (audit high): with CODEGRAPH_MCP_TOKEN set, sessions
// without the matching token in their client hello are dropped; the right
// token is served. Token must never appear in log output (not asserted
// mechanically here, but the code paths never format it).
func TestDaemonTokenAuth(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvMCPToken, "s3cret-token")
	res, err := TryAcquireLock(root)
	if err != nil || res.Kind != "acquired" {
		t.Fatalf("lock: %+v err=%v", res, err)
	}
	defer os.Remove(res.PidPath)

	var sessions atomic.Int32
	handler := func(ctx context.Context, rwc io.ReadWriteCloser) error {
		sessions.Add(1)
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

	sendClientHello := func(conn net.Conn, token string) error {
		ch := ClientHello{CodegraphClient: 1, PID: os.Getpid(), Token: token}
		b, err := json.Marshal(ch)
		if err != nil {
			return err
		}
		_, err = conn.Write(append(b, '\n'))
		return err
	}

	// Case 1: no client hello at all (raw MCP bytes) — cannot authenticate.
	c1, _, _, pr := ConnectHello(d.SocketPath())
	if pr.Outcome != "proxied" {
		t.Fatalf("connect 1: %+v", pr)
	}
	if _, err := c1.Write([]byte("not-a-client-hello\n")); err != nil {
		t.Fatal(err)
	}
	waitConnClosed(t, c1)

	// Case 2: client hello with the wrong token.
	c2, _, _, pr := ConnectHello(d.SocketPath())
	if pr.Outcome != "proxied" {
		t.Fatalf("connect 2: %+v", pr)
	}
	if err := sendClientHello(c2, "wrong-token"); err != nil {
		t.Fatal(err)
	}
	waitConnClosed(t, c2)

	// Case 3: correct token — served normally.
	c3, br3, _, pr := ConnectHello(d.SocketPath())
	if pr.Outcome != "proxied" {
		t.Fatalf("connect 3: %+v", pr)
	}
	defer c3.Close()
	if err := sendClientHello(c3, "s3cret-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := c3.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	_ = c3.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := br3.ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo with valid token: line=%q err=%v", line, err)
	}

	// Rejected sessions must never have run the handler.
	if got := sessions.Load(); got != 1 {
		t.Fatalf("sessions=%d, want 1 (only the authenticated session)", got)
	}
}

// TestDaemonTokenAuthDisabledByDefault: without CODEGRAPH_MCP_TOKEN the
// existing trust model (socket permissions) applies — a hello without a
// token is served (no behavior change for existing deployments).
func TestDaemonTokenAuthDisabledByDefault(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := TryAcquireLock(root)
	if err != nil || res.Kind != "acquired" {
		t.Fatalf("lock: %+v err=%v", res, err)
	}
	defer os.Remove(res.PidPath)

	var sessions atomic.Int32
	handler := func(ctx context.Context, rwc io.ReadWriteCloser) error {
		sessions.Add(1)
		_, _ = io.Copy(io.Discard, rwc)
		return nil
	}
	d := New(root, handler)
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	defer d.Stop("test end")

	conn, _, _, pr := ConnectHello(d.SocketPath())
	if pr.Outcome != "proxied" {
		t.Fatalf("connect: %+v", pr)
	}
	defer conn.Close()
	if err := WriteClientHello(conn); err != nil { // no token in env
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	// Give the daemon a moment to serve the session.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sessions.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if sessions.Load() != 1 {
		t.Fatalf("sessions=%d, want 1 (auth must be off by default)", sessions.Load())
	}
}

// TestDaemonChmodFailureAbortsStart: Start must fail (return an error)
// instead of accepting connections when the socket cannot be chmod'd 0600
// (audit medium: chmod failure used to log and continue Accept).
func TestDaemonChmodFailureAbortsStart(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	oldChmod := chmodSocket
	chmodSocket = func(name string, mode os.FileMode) error { return fmt.Errorf("injected chmod failure") }
	defer func() { chmodSocket = oldChmod }()

	d := New(root, func(ctx context.Context, rwc io.ReadWriteCloser) error { return nil })
	if err := d.Start(); err == nil {
		d.Stop("test cleanup")
		t.Fatal("Start must fail when chmod fails")
	}
	// No socket may remain bound.
	for _, sock := range SocketCandidates(root) {
		if _, serr := os.Stat(sock); !os.IsNotExist(serr) {
			t.Fatalf("socket still present after failed Start: %s", sock)
		}
	}
}

// TestDaemonSocketPermissions0600 verifies that the bound socket file has 0600 permissions.
func TestDaemonSocketPermissions0600(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := TryAcquireLock(root)
	if err != nil || res.Kind != "acquired" {
		t.Fatalf("lock: %+v err=%v", res, err)
	}
	defer os.Remove(res.PidPath)

	d := New(root, func(ctx context.Context, rwc io.ReadWriteCloser) error { return nil })
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	defer d.Stop("test end")

	fi, err := os.Stat(d.SocketPath())
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket permissions = %o, want 0600", perm)
	}
}

// TestDaemonSocketCreationUmaskProtected verifies that the socket inode is created
// with umask 0077 protection at bind time before chmod runs, preventing any window
// with broader-than-0600 / other access.
func TestDaemonSocketCreationUmaskProtected(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := TryAcquireLock(root)
	if err != nil || res.Kind != "acquired" {
		t.Fatalf("lock: %+v err=%v", res, err)
	}
	defer os.Remove(res.PidPath)

	var preChmodPerm os.FileMode
	oldChmod := chmodSocket
	chmodSocket = func(name string, mode os.FileMode) error {
		fi, err := os.Stat(name)
		if err == nil {
			preChmodPerm = fi.Mode().Perm()
		}
		return oldChmod(name, mode)
	}
	defer func() { chmodSocket = oldChmod }()

	d := New(root, func(ctx context.Context, rwc io.ReadWriteCloser) error { return nil })
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	defer d.Stop("test end")

	// Pre-chmod permission must not allow group or other access (0077 umask protection).
	if preChmodPerm&0o077 != 0 {
		t.Fatalf("socket created with broad permissions before chmod: %o (has group/other bits)", preChmodPerm)
	}
	// Post-start permission must be 0600.
	fi, err := os.Stat(d.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("final socket permissions = %o, want 0600", perm)
	}
}

// TestDaemonSocketFallbackPermissions verifies fallback candidate socket creation
// also preserves 0600 permissions and functions normally.
func TestDaemonSocketFallbackPermissions(t *testing.T) {
	long := strings.Repeat("a", 120)
	root := filepath.Join(t.TempDir(), long, "proj")
	if err := os.MkdirAll(CodeGraphDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := TryAcquireLock(root)
	if err != nil || res.Kind != "acquired" {
		t.Fatalf("lock: %+v err=%v", res, err)
	}
	defer os.Remove(res.PidPath)

	d := New(root, func(ctx context.Context, rwc io.ReadWriteCloser) error {
		br := bufio.NewReader(rwc)
		line, _ := br.ReadString('\n')
		_, _ = rwc.Write([]byte(line))
		return nil
	})
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	defer d.Stop("test end")

	// Ensure fallback path (in tmpdir) was selected.
	if !strings.Contains(d.SocketPath(), os.TempDir()) {
		t.Fatalf("expected tmpdir fallback socket, got %s", d.SocketPath())
	}
	fi, err := os.Stat(d.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("fallback socket permissions = %o, want 0600", perm)
	}

	// Verify client proxy connection over fallback socket works.
	conn, br, _, pr := ConnectHello(d.SocketPath())
	if pr.Outcome != "proxied" {
		t.Fatalf("connect fallback socket: %+v", pr)
	}
	defer conn.Close()
	if err := WriteClientHello(conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := br.ReadString('\n')
	if err != nil || resp != "ping\n" {
		t.Fatalf("echo response: got %q, err=%v", resp, err)
	}
}

// TestListenUnixWithUmaskConcurrent verifies concurrent calls to listenUnixWithUmask
// do not race or leak corrupted umask.
func TestListenUnixWithUmaskConcurrent(t *testing.T) {
	dir := t.TempDir()
	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sockPath := filepath.Join(dir, fmt.Sprintf("test-%d.sock", idx))
			ln, err := listenUnixWithUmask(sockPath)
			if err != nil {
				errs <- fmt.Errorf("listen %d: %w", idx, err)
				return
			}
			defer ln.Close()

			fi, err := os.Stat(sockPath)
			if err != nil {
				errs <- fmt.Errorf("stat %d: %w", idx, err)
				return
			}
			if fi.Mode().Perm()&0o077 != 0 {
				errs <- fmt.Errorf("socket %d has broad perm: %o", idx, fi.Mode().Perm())
				return
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
