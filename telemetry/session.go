// Package telemetry records opt-in Scout session outcomes for local analysis.
package telemetry

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

type Event struct {
	Command         string `json:"command"`
	Tool            string `json:"tool,omitempty"`
	Success         bool   `json:"success"`
	ResponseBytes   int    `json:"response_bytes,omitempty"`
	EstimatedTokens int    `json:"estimated_tokens,omitempty"`
	DurationMS      int64  `json:"duration_ms,omitempty"`
}

type Recorder struct {
	mu   sync.Mutex
	file *os.File
}

func NewRecorder(path string) (*Recorder, error) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session telemetry: %w", err)
	}
	return &Recorder{file: file}, nil
}

func (r *Recorder) Record(event Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := r.file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write session telemetry: %w", err)
	}
	return nil
}

func (r *Recorder) Close() error { return r.file.Close() }

type Stats struct {
	Total           int `json:"total"`
	Successful      int `json:"successful"`
	Failed          int `json:"failed"`
	ResponseBytes   int `json:"response_bytes"`
	EstimatedTokens int `json:"estimated_tokens"`
}

func Summarize(r io.Reader) (Stats, error) {
	var stats Stats
	scanner := bufio.NewScanner(r)
	line := 0
	for scanner.Scan() {
		line++
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return Stats{}, fmt.Errorf("decode session telemetry line %d: %w", line, err)
		}
		stats.Total++
		if event.Success {
			stats.Successful++
		} else {
			stats.Failed++
		}
		stats.ResponseBytes += event.ResponseBytes
		stats.EstimatedTokens += event.EstimatedTokens
	}
	if err := scanner.Err(); err != nil {
		return Stats{}, fmt.Errorf("read session telemetry: %w", err)
	}
	return stats, nil
}
