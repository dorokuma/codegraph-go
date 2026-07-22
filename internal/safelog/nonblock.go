// Package safelog provides a non-blocking log writer and a slog-based logger
// setup. The non-blocking writer prevents the process from freezing when
// stderr is a pipe that nobody reads.
package safelog

import (
	"io"
)

// nonBlockWriter drops messages when the async buffer is full instead of blocking.
type nonBlockWriter struct {
	ch chan []byte
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

// Close stops the background goroutine. After Close the writer is no longer usable.
func (w *nonBlockWriter) Close() {
	close(w.ch)
}

func (w *nonBlockWriter) Write(p []byte) (int, error) {
	// log.Logger may reuse the buffer; copy before enqueue.
	b := make([]byte, len(p))
	copy(b, p)
	select {
	case w.ch <- b:
	default:
		// Drop when parent is not draining stderr.
	}
	return len(p), nil
}
