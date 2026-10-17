package policy

import (
	"testing"
	"time"
)

// BenchmarkDecide runs the policy with a full 5-minute stabilization history
// (30 recommendations at a 10s interval) and three signals.
func BenchmarkDecide(b *testing.B) {
	cfg := testConfig()
	cfg.DownStabilization = 5 * time.Minute
	p := New(cfg)
	sig := []Signal{
		{Metric: "cpu", Value: 45, Target: 60},
		{Metric: "rps", Value: 70, Target: 50},
		{Metric: "latency_p95", Value: 120, Target: 200},
	}
	now := t0
	b.ReportAllocs()
	for b.Loop() {
		now = now.Add(10 * time.Second)
		p.Decide(now, 4, sig)
	}
}
