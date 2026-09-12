package safelog

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a mutex-guarded bytes.Buffer so the async drain goroutine and
// the test goroutine can safely share it (race-detector clean).
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestNonBlockWriterDelivers(t *testing.T) {
	var buf syncBuf
	w := newNonBlockWriter(&buf, 16)
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.Close()
	// The background goroutine drains asynchronously — poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if buf.String() == "hello\n" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("message not delivered; buf=%q", buf.String())
}

func TestWriteAfterCloseNoPanic(t *testing.T) {
	// B8: writes after Close must be dropped silently, never panic.
	var buf syncBuf
	w := newNonBlockWriter(&buf, 16)
	w.Close()
	if n, err := w.Write([]byte("late")); err != nil || n != 4 {
		t.Fatalf("write after close: n=%d err=%v", n, err)
	}
	// Close is idempotent.
	w.Close()
}

func TestConcurrentCloseWriteNoPanic(t *testing.T) {
	// B8: Close racing with Write must not panic (atomic flag + recover).
	var buf syncBuf
	w := newNonBlockWriter(&buf, 16)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				_, _ = w.Write([]byte("spam"))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Close()
	}()
	wg.Wait()
	// Ensure the drain goroutine exits (channel closed).
	w.Close()
}

// gateWriter blocks every Write until the gate is opened, and signals the
// first entry, so tests can deterministically pin the drain goroutine inside
// a blocked dst.Write (slow-stderr worst case) before filling the buffer.
type gateWriter struct {
	entered     chan struct{}
	enteredOnce sync.Once
	open        chan struct{}
	openOnce    sync.Once
	mu          sync.Mutex
	lines       []string
}

func newGateWriter() *gateWriter {
	return &gateWriter{entered: make(chan struct{}), open: make(chan struct{})}
}

func (g *gateWriter) Write(p []byte) (int, error) {
	g.enteredOnce.Do(func() { close(g.entered) })
	<-g.open
	g.mu.Lock()
	g.lines = append(g.lines, string(p))
	g.mu.Unlock()
	return len(p), nil
}

func (g *gateWriter) openGate() { g.openOnce.Do(func() { close(g.open) }) }

func (g *gateWriter) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("drain goroutine never entered dst.Write")
	}
}

func (g *gateWriter) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.lines)
}

func (g *gateWriter) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Join(g.lines, "")
}

// TestFlushDrainsPending: N lines written and flushed back-to-back must all
// have reached dst by the time Flush returns — no sleep, no poll. This is
// the contract the fatal exit paths rely on.
func TestFlushDrainsPending(t *testing.T) {
	var buf syncBuf
	w := newNonBlockWriter(&buf, 64)
	const n = 50
	for i := 0; i < n; i++ {
		if _, err := w.Write([]byte(fmt.Sprintf("line-%d\n", i))); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := w.Flush(2 * time.Second); got != 0 {
		t.Fatalf("Flush reported %d entries still pending, want 0", got)
	}
	if got := strings.Count(buf.String(), "\n"); got != n {
		t.Fatalf("dst has %d lines after Flush, want %d; buf=%q", got, n, buf.String())
	}
	w.Close()
}

// TestFlushEmptyWriter: flushing an idle writer returns 0 immediately.
func TestFlushEmptyWriter(t *testing.T) {
	var buf syncBuf
	w := newNonBlockWriter(&buf, 16)
	if got := w.Flush(0); got != 0 { // timeout<=0 → DefaultFlushTimeout, still instant
		t.Fatalf("Flush on empty writer = %d, want 0", got)
	}
	w.Close()
}

// TestDropWhenFullSemanticsUnchanged: with the drain goroutine pinned inside
// a blocked dst.Write, lines beyond the buffer capacity must still be
// dropped (the non-blocking contract must not turn into an unbounded queue),
// and after the gate opens Flush delivers exactly the queued lines.
func TestDropWhenFullSemanticsUnchanged(t *testing.T) {
	g := newGateWriter()
	w := newNonBlockWriter(g, 16)
	if _, err := w.Write([]byte("first\n")); err != nil { // consumed by the goroutine, now blocked in dst.Write
		t.Fatalf("write: %v", err)
	}
	g.waitEntered(t)
	const extra = 20 // 16 fit the buffer, 4 must drop
	for i := 0; i < extra; i++ {
		_, _ = w.Write([]byte(fmt.Sprintf("q%02d\n", i)))
	}
	// 1 line in-flight inside dst.Write + 16 queued; exactly 4 dropped.
	if p := w.pending.Load(); p != 17 {
		t.Fatalf("pending = %d, want 17 (1 in-flight + 16 queued) — drop semantics broken", p)
	}
	g.openGate()
	if got := w.Flush(2 * time.Second); got != 0 {
		t.Fatalf("Flush reported %d pending after gate open, want 0", got)
	}
	if got := g.count(); got != 17 {
		t.Fatalf("dst got %d lines, want 17 (1 in-flight + 16 queued)", got)
	}
	all := g.String()
	for _, want := range []string{"first\n", "q00\n", "q15\n"} {
		if !strings.Contains(all, want) {
			t.Fatalf("dst missing queued line %q; got %q", want, all)
		}
	}
	for _, dropped := range []string{"q16\n", "q19\n"} {
		if strings.Contains(all, dropped) {
			t.Fatalf("dropped line %q was delivered anyway; got %q", dropped, all)
		}
	}
	w.Close()
}

// TestFlushTimeoutReturnsShortfall: with dst stuck, Flush must give up after
// the timeout (never hang) and report the drain shortfall.
func TestFlushTimeoutReturnsShortfall(t *testing.T) {
	g := newGateWriter()
	w := newNonBlockWriter(g, 16)
	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	g.waitEntered(t)
	for i := 0; i < 16; i++ {
		_, _ = w.Write([]byte(fmt.Sprintf("s%02d\n", i)))
	}
	start := time.Now()
	got := w.Flush(100 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Flush blocked for %v, want roughly the 100ms timeout", elapsed)
	}
	if got != 17 {
		t.Fatalf("Flush shortfall = %d, want 17 (1 in-flight + 16 queued)", got)
	}
	g.openGate()
	if got := w.Flush(2 * time.Second); got != 0 {
		t.Fatalf("Flush shortfall after gate open = %d, want 0", got)
	}
	w.Close()
}

// TestCloseFlushesPending is the regression test for the red-team PPID
// watchdog log loss: the proxy path returns normally and its deferred
// cleanup closed the writer without waiting for the async drain, so the
// process could exit before buffered lines reached stderr. Close must
// deliver everything queued before it returns.
func TestCloseFlushesPending(t *testing.T) {
	var buf syncBuf
	w := newNonBlockWriter(&buf, 64)
	const n = 50
	for i := 0; i < n; i++ {
		if _, err := w.Write([]byte(fmt.Sprintf("late-%d\n", i))); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	w.Close() // must flush-and-wait, not just close the channel
	if got := strings.Count(buf.String(), "\n"); got != n {
		t.Fatalf("dst has %d lines right after Close, want %d; buf=%q", got, n, buf.String())
	}
}

// TestFlushAfterCloseWaitsForTail: Flush on a closed writer waits for the
// post-close tail drain and reports 0; late writes stay dropped-and-silent.
func TestFlushAfterCloseWaitsForTail(t *testing.T) {
	var buf syncBuf
	w := newNonBlockWriter(&buf, 32)
	for i := 0; i < 30; i++ {
		_, _ = w.Write([]byte(fmt.Sprintf("tail-%d\n", i)))
	}
	w.Close()
	if got := w.Flush(time.Second); got != 0 {
		t.Fatalf("Flush after Close = %d pending, want 0", got)
	}
	if n, err := w.Write([]byte("post-close\n")); err != nil || n != len("post-close\n") {
		t.Fatalf("write after close: n=%d err=%v", n, err)
	}
	if strings.Contains(buf.String(), "post-close") {
		t.Fatal("write after Close must be dropped")
	}
}

// TestSetupLoggerCleanupFlushes: SetupLogger's cleanup (the deferred
// safelogCleanup in cmd/codegraph-go) must flush queued lines to stderr
// before shutting down — the normal-return exit paths depend on it.
func TestSetupLoggerCleanupFlushes(t *testing.T) {
	r, wp, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = wp
	_, cleanup := SetupLogger("info")
	log.Printf("cleanup-flush-me\n")
	cleanup()
	os.Stderr = oldStderr
	log.SetOutput(os.Stderr)
	wp.Close()

	out := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		out <- string(data)
	}()
	select {
	case got := <-out:
		if !strings.Contains(got, "cleanup-flush-me") {
			t.Fatalf("cleanup dropped the buffered line; got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading flushed stderr")
	}
}
