// Package policy turns metric estimates into a replica count using target
// tracking, a tolerance band, cooldowns and a scale-down stabilization window.
package policy

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Signal is one metric's estimate compared against its per-replica target.
type Signal struct {
	Metric string  `json:"metric"`
	Raw    float64 `json:"raw"`
	Value  float64 `json:"value"`
	Target float64 `json:"target"`
}

type Config struct {
	Min, Max          int
	Tolerance         float64
	UpCooldown        time.Duration
	DownCooldown      time.Duration
	DownStabilization time.Duration
	MaxStepUp         int
	MaxStepDown       int
}

type Decision struct {
	Current     int
	Recommended int // what the metrics alone ask for
	Desired     int // what to actually run after cooldowns and step limits
	Reason      string
}

type rec struct {
	at time.Time
	n  int
}

type Policy struct {
	cfg      Config
	lastUp   time.Time
	lastDown time.Time
	history  []rec
}

func New(cfg Config) *Policy { return &Policy{cfg: cfg} }

func (p *Policy) Decide(now time.Time, current int, signals []Signal) Decision {
	d := Decision{Current: current, Recommended: current, Desired: current}

	// Hard bounds win over cooldowns.
	if current < p.cfg.Min {
		d.Recommended, d.Desired = p.cfg.Min, p.cfg.Min
		d.Reason = fmt.Sprintf("below min %d", p.cfg.Min)
		return d
	}
	if current > p.cfg.Max {
		d.Recommended, d.Desired = p.cfg.Max, p.cfg.Max
		d.Reason = fmt.Sprintf("above max %d", p.cfg.Max)
		return d
	}

	n, why := p.recommend(current, signals)
	if why == "" {
		d.Reason = "no signals"
		return d
	}
	d.Recommended = n
	p.remember(now, n)

	switch {
	case n > current:
		if now.Sub(p.lastUp) < p.cfg.UpCooldown {
			d.Reason = "scale-up cooldown; " + why
			return d
		}
		d.Desired = n
		if p.cfg.MaxStepUp > 0 && d.Desired > current+p.cfg.MaxStepUp {
			d.Desired = current + p.cfg.MaxStepUp
		}
		d.Reason = why
	case n < current:
		// Only shrink to the highest recommendation seen in the window, so a
		// brief dip does not remove capacity that is needed again a minute later.
		stable := p.stabilized()
		if stable >= current {
			d.Reason = fmt.Sprintf("scale-down stabilizing (window max %d); %s", stable, why)
			return d
		}
		if now.Sub(p.lastUp) < p.cfg.DownCooldown || now.Sub(p.lastDown) < p.cfg.DownCooldown {
			d.Reason = "scale-down cooldown; " + why
			return d
		}
		d.Desired = stable
		if p.cfg.MaxStepDown > 0 && d.Desired < current-p.cfg.MaxStepDown {
			d.Desired = current - p.cfg.MaxStepDown
		}
		d.Reason = why
	default:
		d.Reason = "steady; " + why
	}
	return d
}

// Commit records a scaling action that actually happened, starting cooldowns.
func (p *Policy) Commit(now time.Time, from, to int) {
	switch {
	case to > from:
		p.lastUp = now
	case to < from:
		p.lastDown = now
	}
}

// recommend applies target tracking per signal and takes the largest result:
// desired = ceil(current * value / target), unless within the tolerance band.
func (p *Policy) recommend(current int, signals []Signal) (int, string) {
	best := -1
	var parts []string
	for _, s := range signals {
		if s.Target <= 0 {
			continue
		}
		ratio := s.Value / s.Target
		n := current
		if math.Abs(ratio-1) > p.cfg.Tolerance {
			n = int(math.Ceil(float64(current)*ratio - 1e-9))
		}
		parts = append(parts, fmt.Sprintf("%s=%.1f/%.1f->%d", s.Metric, s.Value, s.Target, n))
		if n > best {
			best = n
		}
	}
	if best < 0 {
		return current, ""
	}
	return clamp(best, p.cfg.Min, p.cfg.Max), strings.Join(parts, " ")
}

func (p *Policy) remember(now time.Time, n int) {
	p.history = append(p.history, rec{at: now, n: n})
	cutoff := now.Add(-p.cfg.DownStabilization)
	i := 0
	for i < len(p.history)-1 && p.history[i].at.Before(cutoff) {
		i++
	}
	p.history = p.history[i:]
}

func (p *Policy) stabilized() int {
	m := 0
	for _, r := range p.history {
		if r.n > m {
			m = r.n
		}
	}
	return m
}

func clamp(n, lo, hi int) int {
	return max(lo, min(n, hi))
}
