package memory

import (
	"fmt"
	"sort"
	"time"

	"dockscale/internal/config"
)

// Entry joins a decision with its outcome, if graded yet.
type Entry struct {
	Decision Decision `json:"decision"`
	Outcome  *Outcome `json:"outcome,omitempty"`
}

// Join pairs decisions with outcomes and filters by service ("" for all) and
// time. Entries are returned oldest first.
func Join(records []Record, service string, since time.Time) []Entry {
	outcomes := map[string]*Outcome{}
	for _, r := range records {
		if r.Type == "outcome" && r.Outcome != nil {
			outcomes[r.Outcome.DecisionID] = r.Outcome
		}
	}
	var out []Entry
	for _, r := range records {
		if r.Type != "decision" || r.Decision == nil {
			continue
		}
		d := *r.Decision
		if (service != "" && d.Service != service) || d.At.Before(since) {
			continue
		}
		out = append(out, Entry{Decision: d, Outcome: outcomes[d.ID]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Decision.At.Before(out[j].Decision.At) })
	return out
}

// Report summarizes how well past decisions worked.
type Report struct {
	Service   string         `json:"service"`
	Since     time.Time      `json:"since"`
	Decisions int            `json:"decisions"`
	Ups       int            `json:"ups"`
	Downs     int            `json:"downs"`
	Failed    int            `json:"failed"` // decisions with an error
	Verdicts  map[string]int `json:"verdicts"`
	PerHour   float64        `json:"scale_events_per_hour"`
	Advice    []string       `json:"advice"`
}

// Analyze turns a service's history into a report with tuning advice for
// its current behaviour settings.
func Analyze(entries []Entry, svc config.Service, since, now time.Time) Report {
	r := Report{Service: svc.Name, Since: since, Verdicts: map[string]int{}}
	for _, e := range entries {
		if e.Decision.DryRun {
			continue
		}
		r.Decisions++
		if e.Decision.Up() {
			r.Ups++
		} else {
			r.Downs++
		}
		if e.Decision.Error != "" {
			r.Failed++
		}
		if e.Outcome != nil {
			r.Verdicts[e.Outcome.Verdict]++
		}
	}
	if h := now.Sub(since).Hours(); h > 0 {
		r.PerHour = float64(r.Decisions) / h
	}

	b := svc.Behavior
	gradedUps := r.Verdicts[VerdictEffective] + r.Verdicts[VerdictInsufficient] + r.Verdicts[VerdictUnnecessary]
	gradedDowns := r.Verdicts[VerdictOK] + r.Verdicts[VerdictPremature]
	share := func(n, of int) float64 {
		if of == 0 {
			return 0
		}
		return float64(n) / float64(of)
	}

	const minGraded = 5 // too few graded decisions to judge
	if gradedDowns >= minGraded {
		if p := share(r.Verdicts[VerdictPremature], gradedDowns); p > 0.2 {
			r.Advice = append(r.Advice, fmt.Sprintf(
				"%.0f%% of scale-downs were premature: raise scale_down_stabilization (now %v) or scale_down_cooldown (now %v).",
				100*p, b.ScaleDownStabilization, b.ScaleDownCooldown))
		}
	}
	if gradedUps >= minGraded {
		if p := share(r.Verdicts[VerdictInsufficient], gradedUps); p > 0.3 {
			r.Advice = append(r.Advice, fmt.Sprintf(
				"%.0f%% of scale-ups were insufficient: raise max_step_up (now %d), lower scale_up_cooldown (now %v), or use model holt with a larger horizon.",
				100*p, b.MaxStepUp, b.ScaleUpCooldown))
		}
		if p := share(r.Verdicts[VerdictUnnecessary], gradedUps); p > 0.3 {
			r.Advice = append(r.Advice, fmt.Sprintf(
				"%.0f%% of scale-ups were reversed soon after: raise tolerance (now %.2f), or smooth more (lower alpha, or enable the outlier filter).",
				100*p, b.Tolerance))
		}
	}
	if r.PerHour > 6 {
		r.Advice = append(r.Advice, fmt.Sprintf(
			"%.1f scaling actions per hour suggests flapping: raise tolerance or scale_down_stabilization.", r.PerHour))
	}
	if r.Failed > 0 {
		r.Advice = append(r.Advice, fmt.Sprintf(
			"%d scaling actions failed, so check last_error in /status (image pulls, host capacity).", r.Failed))
	}
	if !svc.History.Prewarm && gradedUps >= minGraded && share(r.Verdicts[VerdictInsufficient], gradedUps) > 0.3 {
		r.Advice = append(r.Advice, "If load follows a daily pattern, enable history.prewarm to add capacity before recurring peaks.")
	}
	if len(r.Advice) == 0 && gradedUps+gradedDowns >= minGraded {
		r.Advice = append(r.Advice, "No tuning issues detected.")
	}
	if gradedUps+gradedDowns < minGraded {
		r.Advice = append(r.Advice, fmt.Sprintf("Only %d graded decisions so far, which is not enough history to advise on.", gradedUps+gradedDowns))
	}
	return r
}
