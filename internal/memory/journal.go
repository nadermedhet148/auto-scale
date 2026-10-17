package memory

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"dockscale/internal/policy"
)

// Decision is one scaling action, as journalled.
type Decision struct {
	ID          string          `json:"id"`
	At          time.Time       `json:"at"`
	Service     string          `json:"service"`
	From        int             `json:"from"`
	To          int             `json:"to"` // replicas actually reached
	Wanted      int             `json:"wanted"`
	Recommended int             `json:"recommended"`
	Reason      string          `json:"reason"`
	Signals     []policy.Signal `json:"signals,omitempty"`
	DryRun      bool            `json:"dry_run,omitempty"`
	Error       string          `json:"error,omitempty"`
}

func (d Decision) Up() bool { return d.To > d.From }

// Verdicts given to decisions once their outcome is known.
const (
	VerdictEffective    = "effective"    // scale-up relieved the overload
	VerdictInsufficient = "insufficient" // scale-up, but load stayed above target
	VerdictUnnecessary  = "unnecessary"  // scale-up undone (count fell back to where it started) within the window
	VerdictOK           = "ok"           // scale-down with no overload after it
	VerdictPremature    = "premature"    // scale-down followed by overload, or undone by a scale-up
)

// Outcome grades a decision after watching what happened for a window.
type Outcome struct {
	DecisionID       string    `json:"decision_id"`
	At               time.Time `json:"at"`
	Service          string    `json:"service"`
	Up               bool      `json:"up"`
	Verdict          string    `json:"verdict"`
	OverloadFraction float64   `json:"overload_fraction"` // share of ticks over target·(1+tolerance)
	PeakRatio        float64   `json:"peak_ratio"`        // highest value/target seen
	Reversed         bool      `json:"reversed"`
	Ticks            int       `json:"ticks"`
}

// Record is one line of the journal file.
type Record struct {
	Type     string    `json:"type"` // "decision" or "outcome"
	Decision *Decision `json:"decision,omitempty"`
	Outcome  *Outcome  `json:"outcome,omitempty"`
}

// NewID returns a sortable, unique-enough ID for a decision.
func NewID(service string, at time.Time) string {
	return service + "-" + strconv.FormatInt(at.UnixNano(), 36)
}

// Evaluator watches each decision for Window and then grades it.
type Evaluator struct {
	Window    time.Duration
	Tolerance float64
	pending   []*watch
}

type watch struct {
	d          Decision
	ticks      int
	overloaded int
	peak       float64
	reversed   bool
}

// Track starts watching a decision. A pending decision is marked reversed
// when this one undoes it: a scale-up whose count falls back to where it
// started, or a scale-down whose count climbs back.
func (e *Evaluator) Track(d Decision) {
	if d.DryRun || d.To == d.From {
		return
	}
	for _, w := range e.pending {
		if (w.d.Up() && d.To <= w.d.From) || (!w.d.Up() && d.To >= w.d.From) {
			w.reversed = true
		}
	}
	e.pending = append(e.pending, &watch{d: d})
}

// Observe feeds one tick of real (non-history) signals and returns the
// outcomes of decisions whose window has ended.
func (e *Evaluator) Observe(now time.Time, signals []policy.Signal) []Outcome {
	over, peak := false, 0.0
	for _, s := range signals {
		if s.Target <= 0 || strings.HasSuffix(s.Metric, HistorySuffix) {
			continue
		}
		r := s.Raw / s.Target
		peak = max(peak, r)
		if r > 1+e.Tolerance {
			over = true
		}
	}

	var done []Outcome
	keep := e.pending[:0]
	for _, w := range e.pending {
		if now.After(w.d.At) {
			w.ticks++
			w.peak = max(w.peak, peak)
			if over {
				w.overloaded++
			}
		}
		if now.Sub(w.d.At) >= e.Window {
			done = append(done, w.grade(now))
		} else {
			keep = append(keep, w)
		}
	}
	e.pending = keep
	return done
}

// Pending returns how many decisions are still being watched.
func (e *Evaluator) Pending() int { return len(e.pending) }

func (w *watch) grade(now time.Time) Outcome {
	o := Outcome{DecisionID: w.d.ID, At: now, Service: w.d.Service, Up: w.d.Up(),
		PeakRatio: w.peak, Reversed: w.reversed, Ticks: w.ticks}
	if w.ticks > 0 {
		o.OverloadFraction = float64(w.overloaded) / float64(w.ticks)
	}
	switch {
	case o.Up && w.reversed:
		o.Verdict = VerdictUnnecessary
	case o.Up && o.OverloadFraction > 0.5:
		o.Verdict = VerdictInsufficient
	case o.Up:
		o.Verdict = VerdictEffective
	case w.reversed || o.OverloadFraction > 0.2:
		o.Verdict = VerdictPremature
	default:
		o.Verdict = VerdictOK
	}
	return o
}

// HistorySuffix marks signals that come from the load profile rather than
// live measurement, e.g. "rps@history".
const HistorySuffix = "@history"

// Describe renders a decision for humans.
func (d Decision) Describe() string {
	s := fmt.Sprintf("%s %s %d→%d", d.At.Format(time.RFC3339), d.Service, d.From, d.To)
	if d.To != d.Wanted {
		s += fmt.Sprintf(" (wanted %d)", d.Wanted)
	}
	if d.DryRun {
		s += " [dry-run]"
	}
	return s + ": " + d.Reason
}
