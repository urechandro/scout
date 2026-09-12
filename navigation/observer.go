package navigation

import "fmt"

// Observer evaluates structured read events and optionally persists their
// metadata. It is deliberately observe-only: callers remain responsible for
// performing the read, regardless of the proposed decision.
type Observer struct {
	Policy   Policy
	Recorder *Recorder
}

// Observe classifies event and records the resulting metadata when a recorder
// is configured. A recording failure is returned for diagnostics, but never
// changes the decision or prevents the caller from continuing the read.
func (o Observer) Observe(event ReadEvent) (Decision, error) {
	decision := o.Policy.Evaluate(event)
	if o.Recorder == nil {
		return decision, nil
	}
	if err := o.Recorder.Record(decision); err != nil {
		return decision, fmt.Errorf("record navigation decision: %w", err)
	}
	return decision, nil
}
