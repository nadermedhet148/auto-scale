package replay

import (
	"bytes"
	"testing"
	"time"

	"dockscale/internal/config"
)

func service() config.Service {
	return config.Service{
		Name: "web", Min: 1, Max: 20,
		Metrics: []config.Metric{{Type: config.MetricRPS, Target: 50}},
		Behavior: config.Behavior{
			Tolerance: 0.1, ScaleUpCooldown: 30 * time.Second, ScaleDownCooldown: time.Minute,
			ScaleDownStabilization: 2 * time.Minute, MaxStepUp: 4, MaxStepDown: 1,
		},
		Model: config.Model{Kind: config.ModelRaw, Alpha: 1, Beta: 1, Gamma: 1, Horizon: 1, OutlierK: -1},
	}
}

func steps(start time.Time, n int, rps func(i int) float64) []Row {
	rows := make([]Row, n)
	for i := range rows {
		rows[i] = Row{At: start.Add(time.Duration(i) * 10 * time.Second), Service: "web",
			Totals: map[string]float64{config.MetricRPS: rps(i)}}
	}
	return rows
}

func TestSimulateFollowsLoad(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 10 min at 400 rps (needs 8 replicas), then 10 min at 50 rps.
	rows := steps(t0, 120, func(i int) float64 {
		if i < 60 {
			return 400
		}
		return 50
	})
	ticks, sum := Simulate(service(), 10*time.Second, rows, Options{})

	if got := ticks[59].Replicas; got != 8 {
		t.Fatalf("replicas at end of high phase = %d, want 8", got)
	}
	if got := ticks[len(ticks)-1].Replicas; got >= 8 {
		t.Fatalf("did not scale down after load dropped: %d", got)
	}
	if sum.ScaleUps == 0 || sum.ScaleDowns == 0 || sum.OverloadTime == 0 {
		t.Fatalf("unexpected summary %+v", sum)
	}
}

func TestStartupDelayExtendsOverload(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := steps(t0, 60, func(int) float64 { return 400 })
	_, fast := Simulate(service(), 10*time.Second, rows, Options{})
	_, slow := Simulate(service(), 10*time.Second, rows, Options{StartupDelay: time.Minute})
	if slow.OverloadTime <= fast.OverloadTime {
		t.Fatalf("startup delay should lengthen overload: fast=%v slow=%v", fast.OverloadTime, slow.OverloadTime)
	}
}

func TestRoundTrip(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := Synthesize("web", SynthOptions{Start: t0, Duration: time.Hour, Interval: 10 * time.Second, Base: 20, Peak: 200, Noise: 0.1, SpikeP: 0.01, Seed: 1}, 0.5)
	var buf bytes.Buffer
	if err := WriteRows(&buf, rows); err != nil {
		t.Fatal(err)
	}
	back, err := ReadRows(&buf, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(rows) || !back[10].At.Equal(rows[10].At) {
		t.Fatalf("round trip lost rows: %d vs %d", len(back), len(rows))
	}
	if d := back[10].Totals[config.MetricRPS] - rows[10].Totals[config.MetricRPS]; d > 0.001 || d < -0.001 {
		t.Fatalf("rps drifted by %v", d)
	}
	if _, ok := back[10].Totals[config.MetricQueueDepth]; ok {
		t.Fatal("empty cell should read as unobserved")
	}
}
