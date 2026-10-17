// Package engine is the per-service decision core shared by the live
// controller and the offline replay tool: per-replica metrics in, replica
// count out. It also owns the service's learned state (models, policy
// memory, load profile) and can snapshot and restore it.
package engine

import (
	"fmt"
	"math"
	"sort"
	"time"

	"dockscale/internal/config"
	"dockscale/internal/memory"
	"dockscale/internal/model"
	"dockscale/internal/policy"
)

type Engine struct {
	cfg     config.Service
	policy  *policy.Policy
	models  map[string]*model.Pipeline
	profile *memory.Profile
}

// New builds an engine. interval is the control-loop period, which sets the
// slot width of seasonal models.
func New(sc config.Service, interval time.Duration) *Engine {
	b, m, h := sc.Behavior, sc.Model, sc.History
	e := &Engine{
		cfg: sc,
		policy: policy.New(policy.Config{
			Min: sc.Min, Max: sc.Max, Tolerance: b.Tolerance,
			UpCooldown: b.ScaleUpCooldown, DownCooldown: b.ScaleDownCooldown,
			DownStabilization: b.ScaleDownStabilization,
			MaxStepUp:         b.MaxStepUp, MaxStepDown: b.MaxStepDown,
		}),
		models: map[string]*model.Pipeline{},
	}
	if h.Cycle > 0 && h.Slot > 0 {
		e.profile = memory.NewProfile(h.Cycle, h.Slot, h.Decay)
	}
	for _, mt := range sc.Metrics {
		e.models[mt.Type] = model.NewPipeline(model.Options{
			Kind: m.Kind, Alpha: m.Alpha, Beta: m.Beta, Gamma: m.Gamma,
			SeasonLength: m.SeasonLength, Interval: interval, Horizon: m.Horizon,
			OutlierK: m.OutlierK, OutlierWindow: m.OutlierWindow,
		})
	}
	return e
}

// Step feeds one tick of per-replica metric values through the models, the
// load profile and the policy. Metrics missing from perReplica are skipped
// this tick.
func (e *Engine) Step(now time.Time, current int, perReplica map[string]float64) ([]policy.Signal, policy.Decision) {
	var signals []policy.Signal
	for _, m := range e.cfg.Metrics {
		v, ok := perReplica[m.Type]
		if !ok {
			continue
		}
		signals = append(signals, policy.Signal{
			Metric: m.Type, Raw: v, Value: e.models[m.Type].Update(now, v), Target: m.Target,
		})
	}

	if e.profile != nil {
		e.profile.Observe(now, Totals(perReplica, current))
		if e.cfg.History.Prewarm && current > 0 {
			signals = append(signals, e.historySignals(now, current)...)
		}
	}
	return signals, e.policy.Decide(now, current, signals)
}

// historySignals asks for the capacity this time slot (or the one Lead
// ahead) usually needs. Only additive metrics can be converted to replicas.
func (e *Engine) historySignals(now time.Time, current int) []policy.Signal {
	h := e.cfg.History
	var out []policy.Signal
	for _, m := range e.cfg.Metrics {
		if !Additive(m.Type) {
			continue
		}
		a, okA := e.profile.Expected(now, m.Type, h.Sigma, h.MinSamples)
		b, okB := e.profile.Expected(now.Add(h.Lead), m.Type, h.Sigma, h.MinSamples)
		if !okA && !okB {
			continue
		}
		total := math.Max(a, b)
		out = append(out, policy.Signal{
			Metric: m.Type + memory.HistorySuffix, Raw: total, Value: total / float64(current), Target: m.Target,
		})
	}
	return out
}

// Commit records a scaling action that actually happened.
func (e *Engine) Commit(now time.Time, from, to int) { e.policy.Commit(now, from, to) }

// Profile returns the learned load profile, or nil if disabled.
func (e *Engine) Profile() *memory.Profile { return e.profile }

// Snapshot is everything the engine has learned, for persistence.
type Snapshot struct {
	Service string                 `json:"service"`
	SavedAt time.Time              `json:"saved_at"`
	Models  map[string]model.State `json:"models"`
	Policy  policy.State           `json:"policy"`
	Profile *memory.ProfileState   `json:"profile,omitempty"`
}

func (e *Engine) Snapshot(now time.Time) Snapshot {
	s := Snapshot{Service: e.cfg.Name, SavedAt: now, Models: map[string]model.State{}, Policy: e.policy.State()}
	for metric, p := range e.models {
		s.Models[metric] = p.State()
	}
	if e.profile != nil {
		ps := e.profile.State()
		s.Profile = &ps
	}
	return s
}

// Restore loads a snapshot. A snapshot older than maxAge restores only
// long-term memory (seasonal offsets, load profile, cooldown times), because
// short-term smoothing state would no longer describe current load. Parts
// that no longer match the config are skipped. It returns notes on what was
// restored and what was skipped, for logging.
func (e *Engine) Restore(s Snapshot, now time.Time, maxAge time.Duration) []string {
	age := now.Sub(s.SavedAt)
	fresh := age <= maxAge
	notes := []string{fmt.Sprintf("snapshot age %v (fresh=%v)", age.Round(time.Second), fresh)}

	metrics := make([]string, 0, len(e.models))
	for m := range e.models {
		metrics = append(metrics, m)
	}
	sort.Strings(metrics)
	for _, metric := range metrics {
		st, ok := s.Models[metric]
		if !ok {
			notes = append(notes, metric+": no saved model state")
			continue
		}
		if err := e.models[metric].Restore(st, fresh); err != nil {
			notes = append(notes, fmt.Sprintf("%s: model state skipped: %v", metric, err))
		}
	}
	e.policy.Restore(s.Policy)
	if e.profile != nil && s.Profile != nil {
		if err := e.profile.Restore(*s.Profile); err != nil {
			notes = append(notes, "profile skipped: "+err.Error())
		}
	}
	return notes
}

// Additive reports whether a metric's total load splits across replicas
// (so per-replica = total / replicas). Latency does not.
func Additive(metric string) bool { return metric != config.MetricLatencyP95 }

// PerReplica converts service-wide totals to per-replica values.
func PerReplica(totals map[string]float64, replicas int) map[string]float64 {
	out := make(map[string]float64, len(totals))
	for k, v := range totals {
		if Additive(k) {
			if replicas <= 0 {
				continue
			}
			v /= float64(replicas)
		}
		out[k] = v
	}
	return out
}

// Totals is the inverse of PerReplica.
func Totals(perReplica map[string]float64, replicas int) map[string]float64 {
	out := make(map[string]float64, len(perReplica))
	for k, v := range perReplica {
		if Additive(k) {
			v *= float64(replicas)
		}
		out[k] = v
	}
	return out
}
