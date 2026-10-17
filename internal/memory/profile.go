// Package memory is what dockscale remembers about the past and how it uses
// it: a load profile per time slot, a journal of scaling decisions graded by
// their outcome, and file storage for both plus learned model state.
//
// Everything except store.go is pure (no I/O, time passed in), so the live
// controller and the replay simulator share it.
package memory

import (
	"fmt"
	"math"
	"time"
)

// Profile learns the typical service-wide load for each Slot of a repeating
// Cycle, e.g. "on a typical day at 14:15, rps is 380 ± 40".
//
// Ticks inside one slot form a visit, and their mean is the visit's value.
// When a visit ends, it updates the slot's mean and variance with weight
// max(1/n, Decay): the first visits are averaged equally, then recent cycles
// count more, so the profile follows gradual change.
type Profile struct {
	Cycle, Slot time.Duration
	Decay       float64

	slots   map[string][]SlotStat // metric -> per-slot statistics
	visits  map[string]*visit     // metric -> the visit in progress
	nslots  int
	lastObs time.Time
}

// SlotStat summarizes past visits to one slot.
type SlotStat struct {
	N    int     `json:"n"`
	Mean float64 `json:"mean"`
	Var  float64 `json:"var"`
}

type visit struct {
	slot int
	sum  float64
	n    int
}

func NewProfile(cycle, slot time.Duration, decay float64) *Profile {
	return &Profile{
		Cycle: cycle, Slot: slot, Decay: decay,
		slots: map[string][]SlotStat{}, visits: map[string]*visit{},
		nslots: int(cycle / slot),
	}
}

// SlotIndex returns the slot that t falls in. Slots are aligned to the Unix
// epoch in UTC, so they are stable across restarts.
func (p *Profile) SlotIndex(t time.Time) int {
	n, c := t.UnixNano(), int64(p.Cycle)
	return int(((n%c)+c)%c/int64(p.Slot)) % p.nslots
}

// Observe records one tick of service-wide totals.
func (p *Profile) Observe(t time.Time, totals map[string]float64) {
	i := p.SlotIndex(t)
	// A gap longer than a slot means the visit in progress is incomplete.
	gap := !p.lastObs.IsZero() && t.Sub(p.lastObs) > p.Slot
	p.lastObs = t
	for metric, x := range totals {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			continue
		}
		v := p.visits[metric]
		if v != nil && (v.slot != i || gap) {
			p.finish(metric, v)
			v = nil
		}
		if v == nil {
			v = &visit{slot: i}
			p.visits[metric] = v
		}
		v.sum += x
		v.n++
	}
}

func (p *Profile) finish(metric string, v *visit) {
	if v.n == 0 {
		return
	}
	slots := p.slots[metric]
	if slots == nil {
		slots = make([]SlotStat, p.nslots)
		p.slots[metric] = slots
	}
	s := &slots[v.slot]
	x := v.sum / float64(v.n)
	s.N++
	w := math.Max(1/float64(s.N), p.Decay)
	d := x - s.Mean
	s.Mean += w * d
	s.Var = (1 - w) * (s.Var + w*d*d)
}

// Expected returns the load this metric usually has in t's slot, plus sigma
// standard deviations of headroom. ok is false until the slot has been
// visited at least minSamples times.
func (p *Profile) Expected(t time.Time, metric string, sigma float64, minSamples int) (float64, bool) {
	slots := p.slots[metric]
	if slots == nil {
		return 0, false
	}
	s := slots[p.SlotIndex(t)]
	if s.N < minSamples {
		return 0, false
	}
	return math.Max(0, s.Mean+sigma*math.Sqrt(s.Var)), true
}

// Slots returns a copy of a metric's per-slot statistics.
func (p *Profile) Slots(metric string) []SlotStat {
	return append([]SlotStat(nil), p.slots[metric]...)
}

// Metrics lists the metrics the profile has learned.
func (p *Profile) Metrics() []string {
	var out []string
	for m := range p.slots {
		out = append(out, m)
	}
	return out
}

// ProfileState is a profile's persisted form. Visits in progress are not
// saved; the visit interrupted by a restart is simply not counted.
type ProfileState struct {
	Cycle time.Duration         `json:"cycle"`
	Slot  time.Duration         `json:"slot"`
	Slots map[string][]SlotStat `json:"slots"`
}

func (p *Profile) State() ProfileState {
	out := ProfileState{Cycle: p.Cycle, Slot: p.Slot, Slots: map[string][]SlotStat{}}
	for m, s := range p.slots {
		out.Slots[m] = append([]SlotStat(nil), s...)
	}
	return out
}

func (p *Profile) Restore(s ProfileState) error {
	if s.Cycle != p.Cycle || s.Slot != p.Slot {
		return fmt.Errorf("profile shape changed from %v/%v to %v/%v", s.Cycle, s.Slot, p.Cycle, p.Slot)
	}
	for m, slots := range s.Slots {
		if len(slots) == p.nslots {
			p.slots[m] = append([]SlotStat(nil), slots...)
		}
	}
	return nil
}
