package main

import (
	"io"
	"log"
	"os"
)

// setupSafeLog routes the standard logger through a non-blocking writer.
// When stderr is a pipe and the parent never reads it, a normal Write would
// block forever and freeze the whole process (including MCP over stdout).
// This writer drops messages when the async buffer is full instead of blocking.
func setupSafeLog() {
	log.SetOutput(newNonBlockWriter(os.Stderr, 256))
	log.SetFlags(log.LstdFlags)
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
