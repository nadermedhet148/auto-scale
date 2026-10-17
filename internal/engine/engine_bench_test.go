package engine

import (
	"testing"
	"time"

	"dockscale/internal/config"
)

// BenchmarkStep is one service's full decision per control-loop tick:
// three metrics through filter + Holt, then the policy.
func BenchmarkStep(b *testing.B) {
	sc := config.Service{
		Name: "web", Min: 1, Max: 20,
		Metrics: []config.Metric{
			{Type: config.MetricCPU, Target: 60}, {Type: config.MetricRPS, Target: 50}, {Type: config.MetricLatencyP95, Target: 200},
		},
		Behavior: config.Behavior{Tolerance: 0.1, ScaleDownStabilization: 5 * time.Minute, MaxStepUp: 4, MaxStepDown: 1},
		Model:    config.Model{Kind: config.ModelHolt, Alpha: 0.5, Beta: 0.3, Horizon: 3, OutlierK: 3.5, OutlierWindow: 6},
	}
	e := New(sc)
	per := map[string]float64{config.MetricCPU: 55, config.MetricRPS: 48, config.MetricLatencyP95: 150}
	now := time.Unix(0, 0)
	b.ReportAllocs()
	for b.Loop() {
		now = now.Add(10 * time.Second)
		e.Step(now, 4, per)
	}
}
