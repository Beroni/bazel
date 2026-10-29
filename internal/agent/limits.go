package agent

import "time"

// Limits is how much of Claude's quota has already been used — the same windows
// that Claude Code's `/usage` shows: the five-hour one (the "session") and the
// seven-day one (the "week").
//
// The data can't be queried: it comes for free in the stream of whoever is
// running, in the `rate_limit_event` event. Outside a review there's nowhere to
// get it from, so what the page shows is always the latest reading — hence At.
type Limits struct {
	Session Window `json:"session"`
	Week    Window `json:"week"`
	// Status is what Claude says about the quota: "allowed" while it can run.
	Status string `json:"status,omitempty"`
	// At is when this reading arrived.
	At time.Time `json:"at"`
}

// Window is a quota window: how much has been used and when it resets.
type Window struct {
	// Utilization goes from 0 to 1.
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at,omitempty"`
}

// Empty says there hasn't been any reading yet.
func (l Limits) Empty() bool { return l.At.IsZero() }

// Percent is the utilization as a whole-number percentage, for the screen.
func (w Window) Percent() int { return int(w.Utilization*100 + 0.5) }
