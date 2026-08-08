// Package safelog provides a non-blocking log writer and a slog-based logger
// setup. The non-blocking writer prevents the process from freezing when
// stderr is a pipe that nobody reads.
//
// Secret redaction (H1): SetupLogger wires two layers —
//  1. a slog.ReplaceAttr that blanks whole values under sensitive keys
//     (token/key/password/secret/authorization/cookie/api_key/session id/…,
//     case-insensitive) and masks known secret shapes inside string values,
//     error messages and the record message;
//  2. a writer-level scrub so legacy log.Printf lines get the same shape
//     masking.
//
// Known shapes: Bearer tokens, Basic auth, JWT (eyJ…), sk-/ghp_/github_pat_/
// xoxb-/AKIA/AIza/stripe keys, PEM private key blocks, and key=value pairs
// with sensitive names (db_password=…).
//
// Capability boundary: this is best-effort, not a guarantee. A secret that
// matches no known shape AND appears under a non-sensitive key (e.g. a
// random-looking 32-char string under "msg"), a purely numeric secret, or a
// multi-line private key split across several legacy log writes can still
// reach the log. Callers must not rely on safelog as the only line of
// defense: avoid logging secrets at the source.
package safelog

import (
	"io"
	"sync"
	"sync/atomic"
)

// nonBlockWriter drops messages when the async buffer is full instead of blocking.
type nonBlockWriter struct {
	ch     chan []byte
	closed atomic.Bool
	// mu serializes Close's channel close against in-flight sends, so a
	// concurrent Close can never race a Write's send (B8).
	mu sync.RWMutex
}

// newNonBlockWriter creates a writer that sends data to dst via a buffered channel.
// If the channel is full, writes are silently dropped.
func newNonBlockWriter(dst io.Writer, queue int) *nonBlockWriter {
	if queue < 16 {
		queue = 16
	}
	w := &nonBlockWriter{ch: make(chan []byte, queue)}
	go func() {
		for b := range w.ch {
			_, _ = dst.Write(b)
		}
	}()
	return w
}

// Close stops the background goroutine. Writes after Close are dropped
// silently (never panic), so late loggers during shutdown are safe (B8).
func (w *nonBlockWriter) Close() {
	if w.closed.CompareAndSwap(false, true) {
		w.mu.Lock()
		close(w.ch)
		w.mu.Unlock()
	}
}

func (w *nonBlockWriter) Write(p []byte) (int, error) {
	// Fast path: after Close, drop silently instead of panicking on a closed
	// channel. This also keeps the common (open) path lock-free.
	if w.closed.Load() {
		return len(p), nil
	}
	// log.Logger may reuse the buffer; copy before enqueue.
	b := make([]byte, len(p))
	copy(b, p)
	// Hold the read lock across the send so Close (which takes the write
	// lock) cannot close the channel mid-send. The closed re-check under the
	// lock closes the Close/Write race window opened by the fast path.
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed.Load() {
		return len(p), nil
	}
	func() {
		// Defense in depth: never panic even if a future code path changes
		// the locking above (B8).
		defer func() { _ = recover() }()
		select {
		case w.ch <- b:
		default:
			// Drop when parent is not draining stderr.
		}
	}()
	return len(p), nil
}
