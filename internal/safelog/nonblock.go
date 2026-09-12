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
	"time"
)

// DefaultFlushTimeout bounds how long Flush and Close wait for the background
// drain goroutine to write pending lines to dst. It is a safety net for a
// slow or stuck stderr (a pipe nobody reads): Flush and Close never block
// longer than this, and the non-blocking Write semantics are unchanged —
// writes are queued or dropped immediately, never delayed by dst.
const DefaultFlushTimeout = 2 * time.Second

// nonBlockWriter drops messages when the async buffer is full instead of blocking.
type nonBlockWriter struct {
	ch chan []byte
	// flushCh carries flush-ack requests: Flush queues a channel and the
	// drain goroutine closes it once every line queued before the request
	// has been handed to dst. Capacity 1 — Flush/Close are serialized by
	// flushMu, so at most one ack is ever in flight.
	flushCh chan chan struct{}
	// done is closed when the drain goroutine exits, letting Close wait for
	// the tail write after the channel is closed.
	done chan struct{}
	// closed flips on Close; writes after it are dropped silently (B8).
	closed atomic.Bool
	// pending counts entries enqueued but not yet handed to dst. Flush
	// reports it as the drain shortfall when it gives up on timeout.
	pending atomic.Int64
	// mu serializes Close's channel close against in-flight sends, so a
	// concurrent Close can never race a Write's send (B8).
	mu sync.RWMutex
	// flushMu serializes Flush/Close so only one flush-ack is in flight.
	flushMu sync.Mutex
}

// newNonBlockWriter creates a writer that sends data to dst via a buffered channel.
// If the channel is full, writes are silently dropped.
func newNonBlockWriter(dst io.Writer, queue int) *nonBlockWriter {
	if queue < 16 {
		queue = 16
	}
	w := &nonBlockWriter{
		ch:      make(chan []byte, queue),
		flushCh: make(chan chan struct{}, 1),
		done:    make(chan struct{}),
	}
	go w.drain(dst)
	return w
}

// drain writes queued lines to dst until the channel is closed and empty.
// The flush-ack branch answers a Flush request only after every line that
// was queued when the request arrived has been written, so a completed
// Flush guarantees dst saw those lines.
func (w *nonBlockWriter) drain(dst io.Writer) {
	defer close(w.done)
	for {
		select {
		case b, ok := <-w.ch:
			if !ok {
				return
			}
			_, _ = dst.Write(b)
			w.pending.Add(-1)
		case ack := <-w.flushCh:
			w.drainAndAck(dst, ack)
		}
	}
}

// drainAndAck writes every queued line, then acknowledges the flush. A line
// enqueued after the final check is not covered by this ack — Flush only
// guarantees lines queued before the request.
func (w *nonBlockWriter) drainAndAck(dst io.Writer, ack chan struct{}) {
	for {
		select {
		case b := <-w.ch:
			_, _ = dst.Write(b)
			w.pending.Add(-1)
		default:
			close(ack)
			return
		}
	}
}

// Flush waits, bounded by timeout, for every line queued in the buffer to be
// written to dst, so callers on fatal paths can lose nothing to the async
// drain. timeout<=0 means DefaultFlushTimeout. It never blocks longer than
// the timeout and never changes the non-blocking Write semantics: concurrent
// writes keep being queued (or dropped when full) as usual.
//
// Returns the number of buffer entries still unwritten when Flush gave up
// (timeout, or the writer was already closed and the tail drain exceeded the
// timeout): 0 means fully drained, >0 is the drain shortfall. Entries queued
// after Flush started count toward it, so under concurrent writers the
// number is best-effort.
func (w *nonBlockWriter) Flush(timeout time.Duration) int {
	if timeout <= 0 {
		timeout = DefaultFlushTimeout
	}
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	return w.flushLocked(timeout)
}

// flushLocked is Flush for callers already holding flushMu (Close).
func (w *nonBlockWriter) flushLocked(timeout time.Duration) int {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	if !w.closed.Load() {
		// The drain goroutine is alive while the channel is open (only Close
		// closes it, under flushMu), so a queued ack is guaranteed to be
		// answered — or the timeout fires first.
		ack := make(chan struct{})
		select {
		case w.flushCh <- ack:
		default:
			// A stale ack from an earlier timed-out Flush is still queued;
			// discard it and requeue ours (best effort — if this also fails
			// we just wait out the timeout below).
			select {
			case <-w.flushCh:
			default:
			}
			select {
			case w.flushCh <- ack:
			default:
			}
		}
		select {
		case <-ack:
			return int(w.pending.Load())
		case <-timer.C:
			return int(w.pending.Load())
		}
	}
	// Closed: the goroutine is finishing the post-close drain; wait for it.
	select {
	case <-w.done:
	case <-timer.C:
	}
	return int(w.pending.Load())
}

// Close flushes pending lines (bounded by DefaultFlushTimeout) before closing
// the channel, then waits, bounded again, for the drain goroutine to finish
// the tail. Without this, a process exiting right after Close raced the
// asynchronous drain and silently lost buffered lines (the red-team PPID
// watchdog log loss). Writes after Close are still dropped silently, never
// panic, so late loggers during shutdown are safe (B8).
func (w *nonBlockWriter) Close() {
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	if w.closed.CompareAndSwap(false, true) {
		// Flush first: the drain goroutine is still alive while the channel
		// is open, so the ack path works here (see flushLocked).
		w.flushLocked(DefaultFlushTimeout)
		w.mu.Lock()
		close(w.ch)
		w.mu.Unlock()
	}
	// Wait (bounded) for the drain goroutine to finish the tail lines.
	select {
	case <-w.done:
	case <-time.After(DefaultFlushTimeout):
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
		// Count the entry before the send so the drain goroutine can never
		// observe (and decrement) it before the increment lands.
		w.pending.Add(1)
		select {
		case w.ch <- b:
		default:
			// Drop when parent is not draining stderr.
			w.pending.Add(-1)
		}
	}()
	return len(p), nil
}
