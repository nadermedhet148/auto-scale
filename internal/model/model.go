// Package model holds the statistical estimators that turn noisy metric
// samples into the value the scaling policy acts on.
package model

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Estimator consumes one sample per tick and returns the value to act on.
// t is the sample's wall-clock time; only seasonal models use it.
type Estimator interface {
	Update(t time.Time, x float64) float64
}

// Raw passes samples through unchanged.
type Raw struct{}

func (Raw) Update(_ time.Time, x float64) float64 { return x }

// EWMA is an exponentially weighted moving average. Higher Alpha reacts
// faster; lower Alpha smooths more.
type EWMA struct {
	Alpha float64
	value float64
	init  bool
}

func (e *EWMA) Update(_ time.Time, x float64) float64 {
	if !e.init {
		e.value, e.init = x, true
		return x
	}
	e.value = e.Alpha*x + (1-e.Alpha)*e.value
	return e.value
}

// Holt is double exponential smoothing (level + trend). It returns a
// forecast Horizon ticks ahead, so a rising load scales out before it peaks.
type Holt struct {
	Alpha, Beta  float64
	Horizon      int
	level, trend float64
	init         bool
}

func (h *Holt) Update(_ time.Time, x float64) float64 {
	if !h.init {
		h.level, h.trend, h.init = x, 0, true
		return x
	}
	prev := h.level
	h.level = h.Alpha*x + (1-h.Alpha)*(h.level+h.trend)
	h.trend = h.Beta*(h.level-prev) + (1-h.Beta)*h.trend
	return math.Max(0, h.level+float64(h.Horizon)*h.trend)
}

// HoltWinters is additive triple exponential smoothing: level + trend + a
// seasonal offset for each Interval-wide slot of a SeasonLength cycle (8640
// slots for a daily cycle at 10s). Slots are aligned to the wall clock, not
// to a tick counter, so gaps and restarts do not shift the season.
//
// The first cycle is spent learning the initial offsets; until then it
// behaves like Holt.
type HoltWinters struct {
	Alpha, Beta, Gamma     float64
	SeasonLength, Interval time.Duration
	Horizon                int

	period       int
	warmup       *Holt
	warm         []float64
	filled       []bool
	seen         int
	ready        bool
	relevel      bool // re-anchor level on the next sample (restored stale state)
	level, trend float64
	season       []float64
}

func (h *HoltWinters) ensure() {
	if h.period == 0 {
		h.period = int(h.SeasonLength / h.Interval)
		h.season = make([]float64, h.period)
		h.warm = make([]float64, h.period)
		h.filled = make([]bool, h.period)
	}
}

func (h *HoltWinters) slot(t time.Time) int {
	n, l := t.UnixNano(), int64(h.SeasonLength)
	return int(((n%l)+l)%l/int64(h.Interval)) % h.period
}

func (h *HoltWinters) Update(t time.Time, x float64) float64 {
	h.ensure()
	i := h.slot(t)
	if !h.ready {
		if h.warmup == nil {
			h.warmup = &Holt{Alpha: h.Alpha, Beta: h.Beta, Horizon: h.Horizon}
		}
		h.warm[i], h.filled[i] = x, true
		h.seen++
		f := h.warmup.Update(t, x)
		if h.seen >= h.period {
			h.initSeason()
		}
		return f
	}

	s := h.season[i]
	if h.relevel {
		h.level, h.trend, h.relevel = x-s, 0, false
	}
	prev := h.level
	h.level = h.Alpha*(x-s) + (1-h.Alpha)*(h.level+h.trend)
	h.trend = h.Beta*(h.level-prev) + (1-h.Beta)*h.trend
	h.season[i] = h.Gamma*(x-h.level) + (1-h.Gamma)*s
	next := h.season[(i+h.Horizon)%h.period]
	return math.Max(0, h.level+float64(h.Horizon)*h.trend+next)
}

func (h *HoltWinters) initSeason() {
	var sum float64
	var n int
	for i, ok := range h.filled {
		if ok {
			sum += h.warm[i]
			n++
		}
	}
	h.level = sum / float64(max(n, 1))
	for i, ok := range h.filled {
		if ok {
			h.season[i] = h.warm[i] - h.level
		}
	}
	h.ready = true
	h.warm, h.filled, h.warmup = nil, nil, nil
}

// OutlierFilter clamps samples further than K scaled MADs from the rolling
// median. Raw samples are kept in the window, so a sustained shift in load
// becomes the new median after about Size/2 ticks and passes through.
type OutlierFilter struct {
	K    float64
	Size int
	buf  []float64
}

const minOutlierSamples = 5

func (f *OutlierFilter) Apply(x float64) float64 {
	out := x
	if len(f.buf) >= minOutlierSamples {
		med := median(f.buf)
		devs := make([]float64, len(f.buf))
		for i, v := range f.buf {
			devs[i] = math.Abs(v - med)
		}
		// 1.4826 makes MAD a consistent estimator of the standard deviation.
		if mad := 1.4826 * median(devs); mad > 0 {
			bound := f.K * mad
			out = math.Min(math.Max(x, med-bound), med+bound)
		}
	}
	f.buf = append(f.buf, x)
	if len(f.buf) > f.Size {
		f.buf = f.buf[1:]
	}
	return out
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

type Options struct {
	Kind          string // raw | ewma | holt | holt_winters
	Alpha, Beta   float64
	Gamma         float64       // holt_winters only
	SeasonLength  time.Duration // holt_winters only
	Interval      time.Duration // holt_winters only: slot width
	Horizon       int
	OutlierK      float64 // <= 0 disables the filter
	OutlierWindow int
}

// Pipeline runs a sample through the outlier filter, then the estimator.
type Pipeline struct {
	kind   string
	filter *OutlierFilter
	est    Estimator
}

func NewPipeline(o Options) *Pipeline {
	p := &Pipeline{kind: o.Kind}
	if o.OutlierK > 0 && o.OutlierWindow > 0 {
		p.filter = &OutlierFilter{K: o.OutlierK, Size: o.OutlierWindow}
	}
	switch o.Kind {
	case "holt_winters":
		p.est = &HoltWinters{Alpha: o.Alpha, Beta: o.Beta, Gamma: o.Gamma,
			SeasonLength: o.SeasonLength, Interval: o.Interval, Horizon: o.Horizon}
	case "holt":
		p.est = &Holt{Alpha: o.Alpha, Beta: o.Beta, Horizon: o.Horizon}
	case "ewma":
		p.est = &EWMA{Alpha: o.Alpha}
	default:
		p.kind = "raw"
		p.est = Raw{}
	}
	return p
}

func (p *Pipeline) Update(t time.Time, x float64) float64 {
	if p.filter != nil {
		x = p.filter.Apply(x)
	}
	return p.est.Update(t, x)
}

// State is a pipeline's learned state, for persistence across restarts.
type State struct {
	Kind   string    `json:"kind"`
	Filter []float64 `json:"filter,omitempty"`

	Init  bool    `json:"init,omitempty"`  // ewma, holt
	Value float64 `json:"value,omitempty"` // ewma
	Level float64 `json:"level,omitempty"` // holt, holt_winters
	Trend float64 `json:"trend,omitempty"` // holt, holt_winters

	SeasonLength time.Duration `json:"season_length,omitempty"` // holt_winters
	Interval     time.Duration `json:"interval,omitempty"`
	Ready        bool          `json:"ready,omitempty"`
	Season       []float64     `json:"season,omitempty"`
	Warm         []float64     `json:"warm,omitempty"`
	Filled       []bool        `json:"filled,omitempty"`
	Seen         int           `json:"seen,omitempty"`
}

func (p *Pipeline) State() State {
	s := State{Kind: p.kind}
	if p.filter != nil {
		s.Filter = append([]float64(nil), p.filter.buf...)
	}
	switch e := p.est.(type) {
	case *EWMA:
		s.Init, s.Value = e.init, e.value
	case *Holt:
		s.Init, s.Level, s.Trend = e.init, e.level, e.trend
	case *HoltWinters:
		s.SeasonLength, s.Interval = e.SeasonLength, e.Interval
		s.Ready, s.Level, s.Trend, s.Seen = e.ready, e.level, e.trend, e.seen
		s.Season = append([]float64(nil), e.season...)
		s.Warm = append([]float64(nil), e.warm...)
		s.Filled = append([]bool(nil), e.filled...)
	}
	return s
}

// Restore loads saved state. When fresh is false (the snapshot is older than
// the caller's max age), short-term state such as the outlier window and the
// smoothed level and trend is dropped. Learned seasonal offsets are always
// kept, because they are aligned to the wall clock and stay valid.
func (p *Pipeline) Restore(s State, fresh bool) error {
	if s.Kind != p.kind {
		return fmt.Errorf("model kind changed from %s to %s", s.Kind, p.kind)
	}
	if fresh && p.filter != nil {
		buf := s.Filter
		if len(buf) > p.filter.Size {
			buf = buf[len(buf)-p.filter.Size:]
		}
		p.filter.buf = append([]float64(nil), buf...)
	}
	switch e := p.est.(type) {
	case *EWMA:
		if fresh {
			e.init, e.value = s.Init, s.Value
		}
	case *Holt:
		if fresh {
			e.init, e.level, e.trend = s.Init, s.Level, s.Trend
		}
	case *HoltWinters:
		if s.SeasonLength != e.SeasonLength || s.Interval != e.Interval {
			return fmt.Errorf("season changed from %v/%v to %v/%v", s.SeasonLength, s.Interval, e.SeasonLength, e.Interval)
		}
		e.ensure()
		switch {
		case s.Ready && len(s.Season) == e.period:
			copy(e.season, s.Season)
			e.ready, e.warm, e.filled, e.seen = true, nil, nil, s.Seen
			if fresh {
				e.level, e.trend = s.Level, s.Trend
			} else {
				e.relevel = true
			}
		case len(s.Warm) == e.period && len(s.Filled) == e.period:
			copy(e.warm, s.Warm)
			copy(e.filled, s.Filled)
			e.seen = s.Seen
		}
	}
	return nil
}
