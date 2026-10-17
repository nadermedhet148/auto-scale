package model

import (
	"math"
	"math/rand/v2"
	"testing"
)

// samples is a fixed noisy series so every benchmark sees identical input.
var samples = func() []float64 {
	rng := rand.New(rand.NewPCG(1, 2))
	xs := make([]float64, 4096)
	for i := range xs {
		xs[i] = 50 + 20*math.Sin(float64(i)/100) + 5*rng.NormFloat64()
	}
	return xs
}()

func benchEstimator(b *testing.B, e Estimator) {
	b.ReportAllocs()
	var sink float64
	for i := 0; b.Loop(); i++ {
		sink += e.Update(samples[i%len(samples)])
	}
	_ = sink
}

func BenchmarkEWMA(b *testing.B) { benchEstimator(b, &EWMA{Alpha: 0.5}) }

func BenchmarkHolt(b *testing.B) { benchEstimator(b, &Holt{Alpha: 0.5, Beta: 0.3, Horizon: 3}) }

// A daily season at a 10s interval: 8640 seasonal slots.
func BenchmarkHoltWintersDaily(b *testing.B) {
	hw := &HoltWinters{Alpha: 0.5, Beta: 0.3, Gamma: 0.2, Period: 8640, Horizon: 3}
	for i := range 8640 {
		hw.Update(samples[i%len(samples)]) // finish warm-up outside the timer
	}
	benchEstimator(b, hw)
}

func BenchmarkOutlierFilter(b *testing.B) {
	for _, size := range []int{6, 30, 120} {
		b.Run(sizeName(size), func(b *testing.B) {
			f := &OutlierFilter{K: 3.5, Size: size}
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				f.Apply(samples[i%len(samples)])
			}
		})
	}
}

func BenchmarkPipelineHoltWithFilter(b *testing.B) {
	benchEstimator(b, NewPipeline(Options{Kind: "holt", Alpha: 0.5, Beta: 0.3, Horizon: 3, OutlierK: 3.5, OutlierWindow: 6}))
}

func sizeName(n int) string {
	switch n {
	case 6:
		return "window=6"
	case 30:
		return "window=30"
	}
	return "window=120"
}
