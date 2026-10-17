package replay

import (
	"math"
	"math/rand/v2"
	"time"

	"dockscale/internal/config"
)

// SynthOptions shapes a synthetic request-rate recording: a daily sine
// wave between Base and Peak rps, Gaussian noise, and occasional spikes.
type SynthOptions struct {
	Start    time.Time
	Duration time.Duration
	Interval time.Duration
	Base     float64 // overnight rps
	Peak     float64 // midday rps
	Noise    float64 // noise std-dev as a fraction of the current level
	SpikeP   float64 // per-tick probability of a short spike
	Seed     uint64
}

// Synthesize generates total rps (and proportional CPU) for one service.
// CPU assumes each request costs a fixed amount, CPUPerRPS percent.
func Synthesize(service string, o SynthOptions, cpuPerRPS float64) []Row {
	rng := rand.New(rand.NewPCG(o.Seed, o.Seed^0x9e3779b97f4a7c15))
	n := int(o.Duration / o.Interval)
	rows := make([]Row, 0, n)
	spikeLeft := 0
	for i := range n {
		at := o.Start.Add(time.Duration(i) * o.Interval)
		hour := float64(at.Hour()) + float64(at.Minute())/60
		// Trough at 03:00, peak at 15:00.
		phase := (1 - math.Cos(2*math.Pi*(hour-3)/24)) / 2
		rps := o.Base + (o.Peak-o.Base)*phase
		rps *= 1 + o.Noise*rng.NormFloat64()
		if spikeLeft == 0 && rng.Float64() < o.SpikeP {
			spikeLeft = 3
		}
		if spikeLeft > 0 {
			rps *= 2.5
			spikeLeft--
		}
		rps = math.Max(0, rps)
		rows = append(rows, Row{
			At: at, Service: service,
			Totals: map[string]float64{config.MetricRPS: rps, config.MetricCPU: rps * cpuPerRPS},
		})
	}
	return rows
}
