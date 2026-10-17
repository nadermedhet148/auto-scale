// Package model holds the statistical estimators that turn noisy metric
// samples into the value the scaling policy acts on.
package model

import (
	"math"
	"sort"
)

// Estimator consumes one sample per tick and returns the value to act on.
type Estimator interface {
	Update(x float64) float64
}

// Raw passes samples through unchanged.
type Raw struct{}

func (Raw) Update(x float64) float64 { return x }

// EWMA is an exponentially weighted moving average. Higher Alpha reacts
// faster; lower Alpha smooths more.
type EWMA struct {
	Alpha float64
	value float64
	init  bool
}

func (e *EWMA) Update(x float64) float64 {
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

func (h *Holt) Update(x float64) float64 {
	if !h.init {
		h.level, h.trend, h.init = x, 0, true
		return x
	}
	prev := h.level
	h.level = h.Alpha*x + (1-h.Alpha)*(h.level+h.trend)
	h.trend = h.Beta*(h.level-prev) + (1-h.Beta)*h.trend
	return math.Max(0, h.level+float64(h.Horizon)*h.trend)
}

// HoltWinters is additive triple exponential smoothing: level + trend +
// a seasonal offset for each of the Period positions in a cycle (for example
// 8640 positions for a daily cycle at a 10s interval). The first cycle is
// spent learning the initial seasonal offsets; until then it behaves like Holt.
type HoltWinters struct {
	Alpha, Beta, Gamma float64
	Period, Horizon    int

	warmup       *Holt
	warm         []float64
	level, trend float64
	season       []float64
	n            int
}

func (h *HoltWinters) Update(x float64) float64 {
	if h.n < h.Period {
		if h.warmup == nil {
			h.warmup = &Holt{Alpha: h.Alpha, Beta: h.Beta, Horizon: h.Horizon}
		}
		h.warm = append(h.warm, x)
		h.n++
		f := h.warmup.Update(x)
		if h.n == h.Period {
			h.initSeason()
		}
		return f
	}

	i := h.n % h.Period
	s := h.season[i]
	prev := h.level
	h.level = h.Alpha*(x-s) + (1-h.Alpha)*(h.level+h.trend)
	h.trend = h.Beta*(h.level-prev) + (1-h.Beta)*h.trend
	h.season[i] = h.Gamma*(x-h.level) + (1-h.Gamma)*s
	h.n++
	next := h.season[(i+h.Horizon)%h.Period]
	return math.Max(0, h.level+float64(h.Horizon)*h.trend+next)
}

func (h *HoltWinters) initSeason() {
	var sum float64
	for _, v := range h.warm {
		sum += v
	}
	h.level = sum / float64(len(h.warm))
	h.season = make([]float64, h.Period)
	for i, v := range h.warm {
		h.season[i] = v - h.level
	}
	h.warm, h.warmup = nil, nil
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
	Gamma         float64 // holt_winters only
	Period        int     // holt_winters: samples per seasonal cycle
	Horizon       int
	OutlierK      float64 // <= 0 disables the filter
	OutlierWindow int
}

// Pipeline runs a sample through the outlier filter, then the estimator.
type Pipeline struct {
	filter *OutlierFilter
	est    Estimator
}

func NewPipeline(o Options) *Pipeline {
	p := &Pipeline{}
	if o.OutlierK > 0 && o.OutlierWindow > 0 {
		p.filter = &OutlierFilter{K: o.OutlierK, Size: o.OutlierWindow}
	}
	switch o.Kind {
	case "holt_winters":
		p.est = &HoltWinters{Alpha: o.Alpha, Beta: o.Beta, Gamma: o.Gamma, Period: o.Period, Horizon: o.Horizon}
	case "holt":
		p.est = &Holt{Alpha: o.Alpha, Beta: o.Beta, Horizon: o.Horizon}
	case "ewma":
		p.est = &EWMA{Alpha: o.Alpha}
	default:
		p.est = Raw{}
	}
	return p
}

func (p *Pipeline) Update(x float64) float64 {
	if p.filter != nil {
		x = p.filter.Apply(x)
	}
	return p.est.Update(x)
}
