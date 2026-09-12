package navigation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// DecodeReadEvent decodes one structured read event. It rejects empty events
// and trailing JSON so hook adapters cannot accidentally process a partial or
// concatenated payload.
func DecodeReadEvent(data []byte) (ReadEvent, error) {
	var event ReadEvent
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&event); err != nil {
		return ReadEvent{}, fmt.Errorf("decode navigation read event: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ReadEvent{}, fmt.Errorf("decode navigation read event: trailing JSON")
		}
		return ReadEvent{}, fmt.Errorf("decode navigation read event: %w", err)
	}
	if event.File == "" {
		return ReadEvent{}, fmt.Errorf("decode navigation read event: file is required")
	}
	return event, nil
}

// ObserveJSON decodes and observes one structured read event. Invalid input
// is rejected before policy evaluation or telemetry recording.
func (o Observer) ObserveJSON(data []byte) (Decision, error) {
	event, err := DecodeReadEvent(data)
	if err != nil {
		return Decision{}, err
	}
	return o.Observe(event)
}
