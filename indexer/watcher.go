package indexer

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/urechandro/scout/protoindexer"
	"github.com/urechandro/scout/store"
	"github.com/urechandro/scout/tsindexer"
)

// Watcher monitors a directory tree for Go and proto file changes and triggers
// reindexing. On each Go save it runs RunFilesLight (~50ms) immediately for
// correct line numbers, then debounces a full RunFiles (~1.5s) for accurate
// call edges. Proto file saves reindex immediately (the parser is fast).
type Watcher struct {
	idx          *Indexer
	protoIdx     *protoindexer.Indexer
	tsIdx        *tsindexer.Indexer
	store        *store.Store
	root         string
	fullDebounce time.Duration

	mu          sync.Mutex
	pendingFull map[string]bool
	fullTimer   *time.Timer

	tsMu      sync.Mutex
	tsChanged bool
	tsTimer   *time.Timer
}

// WatcherConfig controls watcher behavior.
type WatcherConfig struct {
	// Root is the directory tree to watch recursively.
	Root string
	// FullDebounce is the delay before running the full Go reindex after a save.
	// Default: 2 seconds.
	FullDebounce time.Duration
	// TSIndexer is an optional TypeScript indexer. When set, the watcher
	// monitors .ts/.tsx files and triggers debounced reindexing.
	TSIndexer *tsindexer.Indexer
}

// NewWatcher creates a file watcher for the given indexer and store.
func NewWatcher(idx *Indexer, s *store.Store, cfg WatcherConfig) *Watcher {
	debounce := cfg.FullDebounce
	if debounce == 0 {
		debounce = 2 * time.Second
	}
	return &Watcher{
		idx:   idx,
		store: s,
		protoIdx: protoindexer.New(protoindexer.Config{
			Dir: cfg.Root,
		}, s),
		tsIdx:        cfg.TSIndexer,
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

	kinds := ".go and .proto"
	if w.tsIdx != nil {
		kinds += " and .ts/.tsx"
	}
	log.Printf("watcher: monitoring %s for %s file changes", w.root, kinds)

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

	if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
		return
	}

	switch {
	case isGoSource(path):
		w.handleGoChange(path)
	case isProtoSource(path):
		w.handleProtoChange(path)
	case isTSSource(path):
		w.handleTSChange(path)
	}
}

func (w *Watcher) handleGoChange(path string) {
	// Phase 1: fast AST-only reindex (~50ms).
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

func (w *Watcher) handleProtoChange(path string) {
	start := time.Now()
	if err := w.protoIdx.RunFiles([]string{path}); err != nil {
		log.Printf("watcher: proto reindex %s failed: %v", path, err)
		return
	}
	log.Printf("watcher: proto reindex %s (%v)", filepath.Base(path), time.Since(start))
	w.relinkProto("proto")
}

// relinkProto re-runs proto↔Go implements-edge linking. RPC and Go method
// renames both invalidate these edges, so the watcher calls this after either
// a proto reindex or a full Go reindex.
func (w *Watcher) relinkProto(trigger string) {
	linked, err := LinkProtoToGo(w.store)
	if err != nil {
		log.Printf("watcher: proto-go relink after %s failed: %v", trigger, err)
		return
	}
	log.Printf("watcher: proto-go relink after %s (linked %d)", trigger, linked)
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
	w.relinkProto("go")
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

func (w *Watcher) handleTSChange(path string) {
	if w.tsIdx == nil {
		return
	}

	log.Printf("watcher: ts change detected %s", filepath.Base(path))

	w.tsMu.Lock()
	w.tsChanged = true
	if w.tsTimer != nil {
		w.tsTimer.Stop()
	}
	w.tsTimer = time.AfterFunc(w.fullDebounce, w.runTSReindex)
	w.tsMu.Unlock()
}

func (w *Watcher) runTSReindex() {
	w.tsMu.Lock()
	if !w.tsChanged {
		w.tsMu.Unlock()
		return
	}
	w.tsChanged = false
	w.tsMu.Unlock()

	start := time.Now()
	if err := w.tsIdx.Run(); err != nil {
		log.Printf("watcher: ts reindex failed: %v", err)
		return
	}
	log.Printf("watcher: ts reindex (%v)", time.Since(start))
}

func isGoSource(path string) bool {
	return strings.HasSuffix(path, ".go")
}

func isProtoSource(path string) bool {
	return strings.HasSuffix(path, ".proto")
}

func isTSSource(path string) bool {
	if strings.HasSuffix(path, ".d.ts") {
		return false
	}
	return strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx")
}
