package safelog

import (
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// writerChCap is the number of log lines that can be buffered before dropping.
const writerChCap = 256

// w is the global non-blocking writer, kept so SetupLogger's cleanup can shut
// down the goroutine.  Package-level var avoids injecting the writer into every
// call site while keeping the API simple.
var (
	globalWriter *nonBlockWriter
	globalMu     sync.Mutex
)

// SetupLogger creates a non-blocking writer wrapping os.Stderr, configures the
// standard log package to use it (so legacy log.Printf calls are non-blocking),
// and returns a *slog.Logger (also set as the default via slog.SetDefault) and
// a cleanup function that drains and shuts down the background goroutine.
//
// level is a case-insensitive string: "debug", "info", "warn", "error".
// On unrecognised values "info" is used.
func SetupLogger(level string) (*slog.Logger, func()) {
	globalMu.Lock()
	defer globalMu.Unlock()

	// Close any previous writer (defensive; normally called once).
	if globalWriter != nil {
		globalWriter.Close()
	}

	globalWriter = newNonBlockWriter(os.Stderr, writerChCap)

	// Route the standard log package through the non-blocking writer so all
	// existing log.Printf / log.Fatalf calls in lower layers (db, extraction,
	// …) are non-blocking without needing source changes.
	log.SetOutput(globalWriter)
	log.SetFlags(log.LstdFlags)

	// Build a slog.TextHandler that writes to the non-blocking writer.
	levelVar := new(slog.LevelVar)
	levelVar.Set(parseLevel(level))
	textHandler := slog.NewTextHandler(globalWriter, &slog.HandlerOptions{
		Level: levelVar,
	})
	logger := slog.New(textHandler)
	slog.SetDefault(logger)

	cleanup := func() {
		globalMu.Lock()
		w := globalWriter
		globalWriter = nil
		globalMu.Unlock()
		if w != nil {
			w.Close()
		}
	}

	return logger, cleanup
}

// parseLevel converts a string to slog.Level. Defaults to LevelInfo.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// writer returns the current global non-blocking writer, or os.Stderr if unset.
// This is a convenience for tests that need to verify log output.
func writer() io.Writer {
	globalMu.Lock()
	w := globalWriter
	globalMu.Unlock()
	if w != nil {
		return w
	}
	return os.Stderr
}
