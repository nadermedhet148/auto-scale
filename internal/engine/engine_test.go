package engine

import (
	"testing"
	"time"

	"dockscale/internal/config"
)

func TestPerReplicaAndTotalsRoundTrip(t *testing.T) {
	totals := map[string]float64{
		config.MetricCPU: 300, config.MetricRPS: 120, config.MetricQueueDepth: 60, config.MetricLatencyP95: 250,
	}
	per := PerReplica(totals, 3)
	if per[config.MetricRPS] != 40 || per[config.MetricCPU] != 100 || per[config.MetricQueueDepth] != 20 {
		t.Fatalf("per-replica = %v", per)
	}
	if per[config.MetricLatencyP95] != 250 {
		t.Fatal("latency must pass through unchanged")
	}
	back := Totals(per, 3)
	for k, v := range totals {
		if back[k] != v {
			t.Errorf("%s: %v != %v", k, back[k], v)
		}
	}
}

func TestPerReplicaWithZeroReplicasDropsAdditive(t *testing.T) {
	per := PerReplica(map[string]float64{config.MetricRPS: 10, config.MetricLatencyP95: 5}, 0)
	if _, ok := per[config.MetricRPS]; ok || per[config.MetricLatencyP95] != 5 {
		t.Fatalf("per-replica = %v", per)
	}
}

func TestStepSkipsMissingMetricsAndSmooths(t *testing.T) {
	sc := config.Service{
		Name: "web", Min: 1, Max: 10,
		Metrics: []config.Metric{{Type: config.MetricCPU, Target: 50}, {Type: config.MetricRPS, Target: 100}},
		Model:   config.Model{Kind: config.ModelEWMA, Alpha: 0.5},
	}
	e := New(sc)
	now := time.Unix(0, 0)

	sig, _ := e.Step(now, 2, map[string]float64{config.MetricCPU: 40})
	if len(sig) != 1 || sig[0].Metric != config.MetricCPU {
		t.Fatalf("signals = %+v", sig)
	}
	sig, _ = e.Step(now.Add(10*time.Second), 2, map[string]float64{config.MetricCPU: 80, config.MetricRPS: 50})
	if len(sig) != 2 || sig[0].Value != 60 || sig[0].Raw != 80 {
		t.Fatalf("EWMA(40, 80) should be 60: %+v", sig)
	}
}
