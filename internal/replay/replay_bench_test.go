package replay

import (
	"bytes"
	"testing"
	"time"

	"dockscale/internal/config"
)

func threeDays() []Row {
	return Synthesize("web", SynthOptions{
		Start: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), Duration: 72 * time.Hour, Interval: 10 * time.Second,
		Base: 20, Peak: 400, Noise: 0.08, SpikeP: 0.002, Seed: 1,
	}, 0.5)
}

// BenchmarkSimulate replays 3 days at a 10s interval (25,920 ticks) per op.
func BenchmarkSimulate(b *testing.B) {
	rows := threeDays()
	for _, kind := range []string{config.ModelEWMA, config.ModelHolt, config.ModelHW} {
		b.Run(kind, func(b *testing.B) {
			sc := service()
			sc.Metrics = append(sc.Metrics, config.Metric{Type: config.MetricCPU, Target: 60})
			sc.Model = config.Model{Kind: kind, Alpha: 0.5, Beta: 0.3, Gamma: 0.2, SeasonLength: 24 * time.Hour, Horizon: 3, OutlierK: 3.5, OutlierWindow: 6}
			b.ReportAllocs()
			for b.Loop() {
				Simulate(sc, 10*time.Second, rows, Options{StartupDelay: 10 * time.Second})
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(rows)), "ns/tick")
		})
	}
}

func BenchmarkReadRows(b *testing.B) {
	var buf bytes.Buffer
	if err := WriteRows(&buf, threeDays()); err != nil {
		b.Fatal(err)
	}
	data := buf.Bytes()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ReadRows(bytes.NewReader(data), "web"); err != nil {
			b.Fatal(err)
		}
	}
}
