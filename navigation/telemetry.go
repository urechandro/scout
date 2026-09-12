package navigation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RecordedDecision is the privacy-safe event persisted by Recorder. It stores
// classification metadata and the requested path, never source contents.
type RecordedDecision struct {
	Timestamp time.Time `json:"timestamp"`
	Decision
}

// Recorder appends navigation decisions as newline-delimited JSON. The file
// is created with owner-only permissions and writes are serialized for callers
// recording events from concurrent hooks.
type Recorder struct {
	mu   sync.Mutex
	file *os.File
}

// NewRecorder opens path for append, creating its parent directory as needed.
func NewRecorder(path string) (*Recorder, error) {
	if path == "" {
		return nil, fmt.Errorf("navigation telemetry path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create navigation telemetry directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open navigation telemetry: %w", err)
	}
	return &Recorder{file: f}, nil
}

// Record appends one decision and flushes it to the operating system.
func (r *Recorder) Record(d Decision) error {
	if r == nil {
		return fmt.Errorf("navigation telemetry recorder is not initialized")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return fmt.Errorf("navigation telemetry recorder is not initialized")
	}
	entry := RecordedDecision{Timestamp: time.Now().UTC(), Decision: d}
	if err := json.NewEncoder(r.file).Encode(entry); err != nil {
		return fmt.Errorf("write navigation telemetry: %w", err)
	}
	if err := r.file.Sync(); err != nil {
		return fmt.Errorf("flush navigation telemetry: %w", err)
	}
	return nil
}

// Close closes the underlying telemetry file.
func (r *Recorder) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.file.Close()
	r.file = nil
	return err
}
