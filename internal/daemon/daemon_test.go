package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
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
