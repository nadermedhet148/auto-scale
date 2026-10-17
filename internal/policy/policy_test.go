package policy

import (
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		Min: 1, Max: 10, Tolerance: 0.1,
		UpCooldown: 30 * time.Second, DownCooldown: 60 * time.Second,
		DownStabilization: 120 * time.Second,
		MaxStepUp:         4, MaxStepDown: 1,
	}
}

var t0 = time.Unix(1_000_000, 0)

func cpu(v float64) []Signal { return []Signal{{Metric: "cpu", Value: v, Target: 60}} }

func TestTargetTrackingScaleUp(t *testing.T) {
	d := New(testConfig()).Decide(t0, 2, cpu(90))
	if d.Desired != 3 {
		t.Fatalf("got %d, want 3 (%s)", d.Desired, d.Reason)
	}
}

func TestToleranceHolds(t *testing.T) {
	d := New(testConfig()).Decide(t0, 2, cpu(63))
	if d.Desired != 2 {
		t.Fatalf("got %d, want 2 (%s)", d.Desired, d.Reason)
	}
}

func TestMaxStepUp(t *testing.T) {
	d := New(testConfig()).Decide(t0, 2, cpu(600))
	if d.Recommended != 10 || d.Desired != 6 {
		t.Fatalf("got rec=%d desired=%d, want 10/6", d.Recommended, d.Desired)
	}
}

func TestScaleUpCooldown(t *testing.T) {
	p := New(testConfig())
	d := p.Decide(t0, 2, cpu(90))
	p.Commit(t0, 2, d.Desired)

	if d := p.Decide(t0.Add(10*time.Second), 3, cpu(90)); d.Desired != 3 {
		t.Fatalf("scaled during cooldown: %d", d.Desired)
	}
	if d := p.Decide(t0.Add(31*time.Second), 3, cpu(90)); d.Desired != 5 {
		t.Fatalf("got %d after cooldown, want 5", d.Desired)
	}
}

func TestScaleDownStabilization(t *testing.T) {
	p := New(testConfig())
	p.Decide(t0, 4, cpu(60)) // recommends 4

	if d := p.Decide(t0.Add(30*time.Second), 4, cpu(15)); d.Desired != 4 {
		t.Fatalf("scaled down inside stabilization window: %d", d.Desired)
	}
	// The t0 recommendation has aged out; only one replica removed per step.
	if d := p.Decide(t0.Add(130*time.Second), 4, cpu(15)); d.Desired != 3 {
		t.Fatalf("got %d, want 3 (%s)", d.Desired, d.Reason)
	}
}

func TestBounds(t *testing.T) {
	p := New(testConfig())
	if d := p.Decide(t0, 0, nil); d.Desired != 1 {
		t.Fatalf("below min: got %d", d.Desired)
	}
	if d := p.Decide(t0, 12, nil); d.Desired != 10 {
		t.Fatalf("above max: got %d", d.Desired)
	}
}

func TestNoSignalsHolds(t *testing.T) {
	if d := New(testConfig()).Decide(t0, 3, nil); d.Desired != 3 {
		t.Fatalf("got %d, want 3", d.Desired)
	}
}

func TestHighestSignalWins(t *testing.T) {
	sig := []Signal{
		{Metric: "cpu", Value: 60, Target: 60},
		{Metric: "rps", Value: 100, Target: 50},
	}
	if d := New(testConfig()).Decide(t0, 2, sig); d.Desired != 4 {
		t.Fatalf("got %d, want 4", d.Desired)
	}
}

func TestStateRoundTripKeepsCooldown(t *testing.T) {
	p := New(testConfig())
	d := p.Decide(t0, 2, cpu(90))
	p.Commit(t0, 2, d.Desired)

	q := New(testConfig())
	q.Restore(p.State())
	if d := q.Decide(t0.Add(10*time.Second), 3, cpu(90)); d.Desired != 3 {
		t.Fatalf("restored policy ignored the scale-up cooldown: %d", d.Desired)
	}
}
