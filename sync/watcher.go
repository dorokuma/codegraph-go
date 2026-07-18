package sync

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dorokuma/codegraph-go/extraction"

	"github.com/fsnotify/fsnotify"
)

// Watcher watches for file changes and triggers reindexing.
type Watcher struct {
	orchestrator *extraction.Orchestrator
	workdir      string
	watcher      *fsnotify.Watcher
	mu           sync.Mutex
	pending      map[string]time.Time
	debounce     time.Duration
	done         chan struct{}
}

// NewWatcher creates a new file watcher.
func NewWatcher(orch *extraction.Orchestrator, workdir string) (*Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return &Watcher{
		orchestrator: orch,
		workdir:      workdir,
		watcher:      w,
		pending:      make(map[string]time.Time),
		debounce:     2 * time.Second,
		done:         make(chan struct{}),
	}, nil
}

// Start begins watching for file changes.
func (w *Watcher) Start() error {
	// Add directories to watch
	err := filepath.Walk(w.workdir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if extraction.ShouldSkipDir(path, info.Name()) {
				return filepath.SkipDir
			}
			return w.watcher.Add(path)
		}
		return nil
	})
	if err != nil {
		return err
	}

	go w.loop()
	return nil
}

// Stop stops the watcher.
func (w *Watcher) Stop() {
	close(w.done)
	w.watcher.Close()
}

func (w *Watcher) loop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-w.done:
			return

		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}

			// Only care about create, write, remove, rename
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}

			// Check if it's a supported file
			path := event.Name
			lang := extraction.DetectLanguage(path)
			if lang == "" || !extraction.IsSupportedLanguage(lang) {
				continue
			}

			// Add to pending with debounce
			w.mu.Lock()
			w.pending[path] = time.Now()
			w.mu.Unlock()

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)

		case <-ticker.C:
			w.processPending()
		}
	}
}

func (w *Watcher) processPending() {
	w.mu.Lock()
	now := time.Now()
	var ready []string
	for path, t := range w.pending {
		if now.Sub(t) >= w.debounce {
			ready = append(ready, path)
			delete(w.pending, path)
		}
	}
	w.mu.Unlock()

	if len(ready) == 0 {
		return
	}

	// Filter out deleted files
	var existing []string
	for _, path := range ready {
		if _, err := os.Stat(path); err == nil {
			existing = append(existing, path)
		} else {
			// File was deleted, remove from index
			w.orchestrator.DeleteFile(path)
		}
	}

	if len(existing) == 0 {
		return
	}

	// Reindex changed files
	fileCount, nodeCount, err := w.orchestrator.IndexChanges(existing)
	if err != nil {
		log.Printf("sync error: %v", err)
		return
	}

	if fileCount > 0 {
		log.Printf("auto-sync: %d files, %d nodes", fileCount, nodeCount)
	}
}

// PendingFiles returns a list of files waiting to be synced.
func (w *Watcher) PendingFiles() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	var files []string
	for path := range w.pending {
		rel, _ := filepath.Rel(w.workdir, path)
		if rel == "" {
			rel = path
		}
		files = append(files, rel)
	}
	return files
}

// AddDir adds a new directory to the watch list.
func (w *Watcher) AddDir(path string) error {
	return w.watcher.Add(path)
}

// RemoveDir removes a directory from the watch list.
func (w *Watcher) RemoveDir(path string) error {
	return w.watcher.Remove(path)
}

// IsSupported returns true if the file is a supported source file.
func IsSupported(path string) bool {
	lang := extraction.DetectLanguage(path)
	return lang != "" && extraction.IsSupportedLanguage(lang)
}
