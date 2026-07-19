package db

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// WALCheckpoint manages periodic WAL checkpoints to prevent unbounded WAL growth.
// In WAL mode, readers never block writers, but the WAL file grows until a
// checkpoint runs. Without periodic checkpoints, the WAL can grow to gigabytes
// on busy indexes.
//
// Ported from official db/wal-valve.ts concept.
type WALCheckpoint struct {
	db       *DB
	interval time.Duration
	stop     chan struct{}
	done     sync.WaitGroup
}

// NewWALCheckpoint creates a checkpoint manager that runs PRAGMA wal_checkpoint(PASSIVE)
// at the given interval. PASSIVE never blocks readers — it checkpoints as much
// as it can without waiting.
//
// A nil DB or zero/negative interval disables checkpoints.
func NewWALCheckpoint(database *DB, interval time.Duration) *WALCheckpoint {
	if database == nil || interval <= 0 {
		return nil
	}
	return &WALCheckpoint{
		db:       database,
		interval: interval,
		stop:     make(chan struct{}),
	}
}

// Start begins the background checkpoint loop. Non-blocking.
func (w *WALCheckpoint) Start() {
	if w == nil {
		return
	}
	w.done.Add(1)
	go func() {
		defer w.done.Done()
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.runCheckpoint()
			case <-w.stop:
				return
			}
		}
	}()
}

// Stop halts the checkpoint loop and waits for it to finish.
func (w *WALCheckpoint) Stop() {
	if w == nil {
		return
	}
	close(w.stop)
	w.done.Wait()
}

// runCheckpoint runs a passive WAL checkpoint. Logs failures but never interrupts work.
func (w *WALCheckpoint) runCheckpoint() {
	w.db.mu.Lock()
	defer w.db.mu.Unlock()

	// PASSIVE: checkpoint as much as possible without blocking readers.
	// Returns (busy, log) — we ignore busy (readers are fine).
	rows, err := w.db.conn.Query("PRAGMA wal_checkpoint(PASSIVE)")
	if err != nil {
		log.Printf("wal checkpoint: %v", err)
		return
	}
	defer rows.Close()
	// Consume result to avoid "query not finished" errors.
	for rows.Next() {
	}
}

// WALSize returns the current WAL file size in bytes, or 0 on error.
func (d *DB) WALSize() int64 {
	walPath := d.path + "-wal"
	fi, err := os.Stat(walPath)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// WALInfo returns human-readable WAL status for diagnostics.
func (d *DB) WALInfo() string {
	size := d.WALSize()
	if size == 0 {
		return "WAL: empty or missing"
	}
	return fmt.Sprintf("WAL: %d bytes", size)
}
