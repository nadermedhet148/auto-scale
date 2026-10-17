package engine

import (
	"strings"
	"testing"
	"time"

	"dockscale/internal/config"
	"dockscale/internal/policy"
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
	e := New(sc, 10*time.Second)
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

func webWithHistory(prewarm bool) config.Service {
	return config.Service{
		Name: "web", Min: 1, Max: 20,
		Metrics:  []config.Metric{{Type: config.MetricRPS, Target: 50}},
		Behavior: config.Behavior{Tolerance: 0.1},
		Model:    config.Model{Kind: config.ModelEWMA, Alpha: 0.5},
		History: config.History{Prewarm: prewarm, Cycle: 24 * time.Hour, Slot: 15 * time.Minute,
			Lead: 5 * time.Minute, Sigma: 0, MinSamples: 2, Decay: 0.25},
	}
}

// learnPeak runs days of a 14:00-15:00 peak of 500 rps total (50 otherwise)
// through e at a fixed 2 replicas.
func learnPeak(e *Engine, start time.Time, days int) {
	for t := start; t.Before(start.Add(time.Duration(days) * 24 * time.Hour)); t = t.Add(10 * time.Second) {
		total := 50.0
		if t.Hour() == 14 {
			total = 500
		}
		e.Step(t, 2, PerReplica(map[string]float64{config.MetricRPS: total}, 2))
	}
}

func TestPrewarmAsksForUsualCapacityBeforePeak(t *testing.T) {
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	e := New(webWithHistory(true), 10*time.Second)
	learnPeak(e, start, 3)

	// 13:57 on day 4: live load is still low, but 14:00 is 3 minutes away.
	now := start.Add(3*24*time.Hour + 13*time.Hour + 57*time.Minute)
	sig, d := e.Step(now, 2, PerReplica(map[string]float64{config.MetricRPS: 50}, 2))
	var hist *policy.Signal
	for i := range sig {
		if sig[i].Metric == config.MetricRPS+"@history" {
			hist = &sig[i]
		}
	}
	if hist == nil || hist.Raw != 500 {
		t.Fatalf("signals = %+v, want an rps@history signal expecting 500", sig)
	}
	if d.Recommended != 10 {
		t.Fatalf("recommended %d, want 10 (500 rps / 50 per replica)", d.Recommended)
	}

	off := New(webWithHistory(false), 10*time.Second)
	learnPeak(off, start, 3)
	if _, d := off.Step(now, 2, PerReplica(map[string]float64{config.MetricRPS: 50}, 2)); d.Recommended >= 10 {
		t.Fatalf("prewarm off should not pre-scale: recommended %d", d.Recommended)
	}
}

func TestSnapshotRestoreKeepsProfileAndCooldowns(t *testing.T) {
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	e := New(webWithHistory(true), 10*time.Second)
	learnPeak(e, start, 3)
	at := start.Add(3 * 24 * time.Hour)
	e.Commit(at, 2, 4)
	snap := e.Snapshot(at)

	// A fresh process restores a day-old snapshot: stale, but the profile
	// and the cooldown times are long-term memory and survive.
	f := New(webWithHistory(true), 10*time.Second)
	notes := f.Restore(snap, at.Add(24*time.Hour), 15*time.Minute)
	if len(notes) == 0 || !strings.Contains(notes[0], "fresh=false") {
		t.Fatalf("notes = %v", notes)
	}
	if v, ok := f.Profile().Expected(start.Add(14*time.Hour), config.MetricRPS, 0, 2); !ok || v != 500 {
		t.Fatalf("profile not restored: %v %v", v, ok)
	}
	if got := f.policy.State().LastUp; !got.Equal(at) {
		t.Fatalf("policy last scale-up = %v, want %v", got, at)
	}
}
