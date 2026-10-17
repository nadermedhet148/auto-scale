package replay

import (
	"sort"
	"time"

	"dockscale/internal/config"
	"dockscale/internal/engine"
	"dockscale/internal/policy"
)

type Options struct {
	Start        int           // initial replicas; 0 means the service's min
	StartupDelay time.Duration // time before a new replica takes load
}

// Tick is one simulated control-loop iteration.
type Tick struct {
	At       time.Time
	Replicas int // counted by the policy, including those still starting
	Ready    int // replicas carrying load
	Desired  int
	Signals  []policy.Signal
	Reason   string
	Overload bool // some metric's per-ready-replica load exceeded target·(1+tolerance)
}

type Summary struct {
	Ticks           int
	Duration        time.Duration
	ScaleUps        int
	ScaleDowns      int
	PeakReplicas    int
	ReplicaHours    float64
	OverloadTime    time.Duration
	RecordedHours   float64 // replica-hours actually run, if the recording has replica counts
	MedianStep      time.Duration
	IntervalWarning bool // recording spacing differs from the configured interval
}

// Simulate replays recorded load through a fresh engine. Scale actions take
// effect immediately for the count the policy sees, but new replicas only
// share load after StartupDelay.
func Simulate(sc config.Service, interval time.Duration, rows []Row, opt Options) ([]Tick, Summary) {
	var sum Summary
	if len(rows) == 0 {
		return nil, sum
	}
	eng := engine.New(sc)
	current := opt.Start
	if current <= 0 {
		current = sc.Min
	}
	var pending []time.Time // ready-at times of replicas still starting
	ticks := make([]Tick, 0, len(rows))

	for i, row := range rows {
		now := row.At
		for len(pending) > 0 && !pending[0].After(now) {
			pending = pending[1:]
		}
		ready := current - len(pending)
		perReplica := engine.PerReplica(row.Totals, max(ready, 1))

		signals, d := eng.Step(now, current, perReplica)
		t := Tick{At: now, Replicas: current, Ready: ready, Desired: d.Desired, Signals: signals, Reason: d.Reason}
		for _, m := range sc.Metrics {
			if v, ok := perReplica[m.Type]; ok && v > m.Target*(1+sc.Behavior.Tolerance) {
				t.Overload = true
			}
		}
		ticks = append(ticks, t)

		var dt time.Duration
		if i+1 < len(rows) {
			dt = rows[i+1].At.Sub(now)
		}
		sum.ReplicaHours += float64(current) * dt.Hours()
		sum.RecordedHours += float64(row.Replicas) * dt.Hours()
		if t.Overload {
			sum.OverloadTime += dt
		}
		sum.PeakReplicas = max(sum.PeakReplicas, current)

		if d.Desired > current {
			sum.ScaleUps++
			for range d.Desired - current {
				pending = append(pending, now.Add(opt.StartupDelay))
			}
		} else if d.Desired < current {
			sum.ScaleDowns++
			// Stop starting replicas first, newest first, as the controller would.
			drop := current - d.Desired
			for drop > 0 && len(pending) > 0 {
				pending = pending[:len(pending)-1]
				drop--
			}
		}
		if d.Desired != current {
			eng.Commit(now, current, d.Desired)
			current = d.Desired
		}
	}

	sum.Ticks = len(rows)
	sum.Duration = rows[len(rows)-1].At.Sub(rows[0].At)
	sum.MedianStep = medianStep(rows)
	if sum.MedianStep > 0 {
		r := float64(sum.MedianStep) / float64(interval)
		sum.IntervalWarning = r < 0.8 || r > 1.25
	}
	return ticks, sum
}

func medianStep(rows []Row) time.Duration {
	if len(rows) < 2 {
		return 0
	}
	steps := make([]time.Duration, 0, len(rows)-1)
	for i := 1; i < len(rows); i++ {
		steps = append(steps, rows[i].At.Sub(rows[i-1].At))
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i] < steps[j] })
	return steps[len(steps)/2]
}
