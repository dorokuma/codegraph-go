package safelog

import (
	"bytes"
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
