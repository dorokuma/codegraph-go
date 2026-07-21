package main

import (
	"io"
	"log"
	"os"
	"sync"
)

// safeWriter holds the nonBlockWriter so cleanup can Close it.
var safeWriter *nonBlockWriter
var safeWriterOnce sync.Once

// setupSafeLog routes the standard logger through a non-blocking writer.
// When stderr is a pipe and the parent never reads it, a normal Write would
// block forever and freeze the whole process (including MCP over stdout).
// This writer drops messages when the async buffer is full instead of blocking.
func setupSafeLog() {
	sw := newNonBlockWriter(os.Stderr, 256)
	safeWriter = sw
	log.SetOutput(sw)
	log.SetFlags(log.LstdFlags)
}

// closeSafeLog gracefully shuts down the non-blocking writer goroutine.
// Safe to call multiple times (sync.Once).
func closeSafeLog() {
	safeWriterOnce.Do(func() {
		if safeWriter != nil {
			safeWriter.Close()
		}
	})
}

type nonBlockWriter struct {
	ch chan []byte
}

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
