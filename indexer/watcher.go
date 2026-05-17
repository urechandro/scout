package indexer

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher monitors a directory tree for Go file changes and triggers
// reindexing. On each save it runs RunFilesLight (~1ms) immediately for
// correct line numbers, then debounces a full RunFiles (~1.5s) for
// accurate call edges and type information.
type Watcher struct {
	idx          *Indexer
	root         string
	debounce     time.Duration
	fullDebounce time.Duration

	mu           sync.Mutex
	pendingFull  map[string]bool
	fullTimer    *time.Timer
}

// WatcherConfig controls watcher behavior.
type WatcherConfig struct {
	// Root is the directory tree to watch recursively.
	Root string
	// Debounce is the delay before running the full reindex after a save.
	// Default: 2 seconds.
	FullDebounce time.Duration
}

// NewWatcher creates a file watcher for the given indexer.
func NewWatcher(idx *Indexer, cfg WatcherConfig) *Watcher {
	debounce := cfg.FullDebounce
	if debounce == 0 {
		debounce = 2 * time.Second
	}
	return &Watcher{
		idx:          idx,
		root:         cfg.Root,
		fullDebounce: debounce,
		pendingFull:  make(map[string]bool),
	}
}

// Run starts watching and blocks until an error occurs or the process exits.
func (w *Watcher) Run() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := w.addDirs(watcher); err != nil {
		return err
	}

	log.Printf("watcher: monitoring %s for .go file changes", w.root)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			w.handleEvent(event, watcher)
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Printf("watcher error: %v", err)
		}
	}
}

func (w *Watcher) handleEvent(event fsnotify.Event, watcher *fsnotify.Watcher) {
	path := event.Name

	// Watch new directories as they appear.
	if event.Has(fsnotify.Create) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			if !w.shouldSkipDir(filepath.Base(path)) {
				_ = watcher.Add(path)
			}
			return
		}
	}

	if !isGoSource(path) {
		return
	}

	if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
		return
	}

	// Phase 1: fast AST-only reindex (~1ms).
	start := time.Now()
	if err := w.idx.RunFilesLight([]string{path}); err != nil {
		log.Printf("watcher: light reindex %s failed: %v", path, err)
		return
	}
	log.Printf("watcher: light reindex %s (%v)", filepath.Base(path), time.Since(start))

	// Phase 2: queue full reindex with debounce.
	w.mu.Lock()
	w.pendingFull[path] = true
	if w.fullTimer != nil {
		w.fullTimer.Stop()
	}
	w.fullTimer = time.AfterFunc(w.fullDebounce, w.runFullReindex)
	w.mu.Unlock()
}

func (w *Watcher) runFullReindex() {
	w.mu.Lock()
	files := make([]string, 0, len(w.pendingFull))
	for f := range w.pendingFull {
		files = append(files, f)
	}
	w.pendingFull = make(map[string]bool)
	w.mu.Unlock()

	if len(files) == 0 {
		return
	}

	start := time.Now()
	if err := w.idx.RunFiles(files); err != nil {
		log.Printf("watcher: full reindex failed: %v", err)
		return
	}
	log.Printf("watcher: full reindex %d files (%v)", len(files), time.Since(start))
}

func (w *Watcher) addDirs(watcher *fsnotify.Watcher) error {
	return filepath.WalkDir(w.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if w.shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return watcher.Add(path)
		}
		return nil
	})
}

func (w *Watcher) shouldSkipDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "vendor", "node_modules", "testdata":
		return true
	}
	return false
}

func isGoSource(path string) bool {
	return strings.HasSuffix(path, ".go")
}
