package daemon

import (
	"log"
	"os"
	"sync"
	"time"
)

// StartPPIDWatchdog exits the process when the original parent dies
// (host SIGKILL path). interval<=0 disables. Returns a stop func.
func StartPPIDWatchdog(interval time.Duration, onLost func()) (stop func()) {
	if interval <= 0 {
		return func() {}
	}
	orig := os.Getppid()
	if orig <= 1 {
		// Already reparented — don't arm (tests / orphaned start).
		return func() {}
	}
	var once sync.Once
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				cur := os.Getppid()
				if cur != orig || !IsProcessAlive(orig) {
					once.Do(func() {
						log.Printf("parent process exited (ppid %d→%d); shutting down", orig, cur)
						if onLost != nil {
							onLost()
						}
					})
					return
				}
			}
		}
	}()
	return func() { close(done) }
}
