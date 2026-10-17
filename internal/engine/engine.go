// Package engine is the per-service decision core shared by the live
// controller and the offline replay tool: per-replica metrics in, replica
// count out.
package engine

import (
	"time"

	"dockscale/internal/config"
	"dockscale/internal/model"
	"dockscale/internal/policy"
)

type Engine struct {
	cfg    config.Service
	policy *policy.Policy
	models map[string]*model.Pipeline
}

func New(sc config.Service) *Engine {
	b, m := sc.Behavior, sc.Model
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
	for _, mt := range sc.Metrics {
		e.models[mt.Type] = model.NewPipeline(model.Options{
			Kind: m.Kind, Alpha: m.Alpha, Beta: m.Beta, Gamma: m.Gamma,
			Period: m.Period, Horizon: m.Horizon,
			OutlierK: m.OutlierK, OutlierWindow: m.OutlierWindow,
		})
	}
	return e
}

// Step feeds one tick of per-replica metric values through the models and
// the policy. Metrics missing from perReplica are skipped this tick.
func (e *Engine) Step(now time.Time, current int, perReplica map[string]float64) ([]policy.Signal, policy.Decision) {
	var signals []policy.Signal
	for _, m := range e.cfg.Metrics {
		v, ok := perReplica[m.Type]
		if !ok {
			continue
		}
		signals = append(signals, policy.Signal{
			Metric: m.Type, Raw: v, Value: e.models[m.Type].Update(v), Target: m.Target,
		})
	}
	return signals, e.policy.Decide(now, current, signals)
}

// Commit records a scaling action that actually happened.
func (e *Engine) Commit(now time.Time, from, to int) { e.policy.Commit(now, from, to) }

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
