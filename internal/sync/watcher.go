package sync

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dorokuma/codegraph-go/internal/extraction"

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
	stopOnce     sync.Once
	wg           sync.WaitGroup
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
			if extraction.ShouldSkipDirIn(w.workdir, path, info.Name()) {
				return filepath.SkipDir
			}
			return w.watcher.Add(path)
		}
		return nil
	})
	if err != nil {
		return err
	}

	w.wg.Add(1)
	go w.loop()
	return nil
}

// Stop stops the watcher. Safe to call multiple times.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.done)
		_ = w.watcher.Close()
	})
	w.wg.Wait()
}

func (w *Watcher) loop() {
	defer w.wg.Done()
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

			path := event.Name

			// New directories must be watched recursively (official watcher behavior).
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(path); err == nil && info.IsDir() {
					w.watchTree(path)
					continue
				}
			}

			// Supported source files only
			lang := extraction.DetectLanguage(path)
			if lang == "" || !extraction.IsSupportedLanguage(lang) {
				continue
			}

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

// watchTree recursively adds a newly created directory tree to the watch list.
func (w *Watcher) watchTree(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			// Queue source files that appeared with the new tree
			lang := extraction.DetectLanguage(path)
			if lang != "" && extraction.IsSupportedLanguage(lang) {
				w.mu.Lock()
				w.pending[path] = time.Now()
				w.mu.Unlock()
			}
			return nil
		}
		if extraction.ShouldSkipDirIn(w.workdir, path, info.Name()) {
			return filepath.SkipDir
		}
		if err := w.watcher.Add(path); err != nil {
			log.Printf("watcher add %s: %v", path, err)
		}
		return nil
	})
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
		} else if os.IsNotExist(err) {
			// File was deleted, remove from index
			if err := w.orchestrator.DeleteFile(path); err != nil {
				log.Printf("delete index %s: %v", path, err)
			}
		} else {
			// Permission error or other transient issue — skip, don't delete
			log.Printf("stat pending %s: %v", path, err)
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

	const maxPendingFiles = 500
	files := make([]string, 0, maxPendingFiles)
	for path := range w.pending {
		rel, _ := filepath.Rel(w.workdir, path)
		if rel == "" {
			rel = path
		}
		files = append(files, rel)
		if len(files) >= maxPendingFiles {
			break
		}
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
