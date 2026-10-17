package memory

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockscale/internal/config"
	"dockscale/internal/policy"
)

var day0 = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

// feed observes a value every 10s for d starting at from.
func feed(p *Profile, from time.Time, d time.Duration, metric string, value func(t time.Time) float64) {
	for t := from; t.Before(from.Add(d)); t = t.Add(10 * time.Second) {
		p.Observe(t, map[string]float64{metric: value(t)})
	}
}

func TestProfileLearnsDailyPattern(t *testing.T) {
	p := NewProfile(24*time.Hour, 15*time.Minute, 0.25)
	peakAt14 := func(t time.Time) float64 {
		if t.Hour() == 14 {
			return 400
		}
		return 50
	}
	feed(p, day0, 3*24*time.Hour, "rps", peakAt14)

	at := day0.Add(3*24*time.Hour + 14*time.Hour + 5*time.Minute)
	if v, ok := p.Expected(at, "rps", 0, 2); !ok || math.Abs(v-400) > 1e-6 {
		t.Fatalf("expected at 14:05 = %v, %v; want 400", v, ok)
	}
	if v, ok := p.Expected(at.Add(-2*time.Hour), "rps", 0, 2); !ok || math.Abs(v-50) > 1e-6 {
		t.Fatalf("expected at 12:05 = %v, %v; want 50", v, ok)
	}
}

func TestProfileNeedsMinSamples(t *testing.T) {
	p := NewProfile(24*time.Hour, 15*time.Minute, 0.25)
	feed(p, day0, 24*time.Hour, "rps", func(time.Time) float64 { return 10 })
	if _, ok := p.Expected(day0.Add(25*time.Hour), "rps", 0, 2); ok {
		t.Fatal("one visit per slot should not be trusted with min_samples 2")
	}
}

func TestProfileSigmaAddsHeadroom(t *testing.T) {
	p := NewProfile(24*time.Hour, 15*time.Minute, 0.25)
	// Alternate days: 100 then 200 in every slot.
	for d := range 4 {
		v := 100.0 + 100*float64(d%2)
		feed(p, day0.Add(time.Duration(d)*24*time.Hour), 24*time.Hour, "rps", func(time.Time) float64 { return v })
	}
	at := day0.Add(4*24*time.Hour + time.Hour)
	mean, _ := p.Expected(at, "rps", 0, 2)
	high, _ := p.Expected(at, "rps", 1, 2)
	if !(mean > 100 && mean < 200) || high <= mean {
		t.Fatalf("mean=%v high=%v; want 100 < mean < high", mean, high)
	}
}

func TestProfileStateRoundTrip(t *testing.T) {
	p := NewProfile(24*time.Hour, 15*time.Minute, 0.25)
	feed(p, day0, 3*24*time.Hour, "rps", func(time.Time) float64 { return 42 })
	q := NewProfile(24*time.Hour, 15*time.Minute, 0.25)
	if err := q.Restore(p.State()); err != nil {
		t.Fatal(err)
	}
	if v, ok := q.Expected(day0.Add(time.Hour), "rps", 0, 2); !ok || v != 42 {
		t.Fatalf("restored expected = %v, %v", v, ok)
	}
	if err := NewProfile(168*time.Hour, 15*time.Minute, 0.25).Restore(p.State()); err == nil {
		t.Fatal("restoring a daily profile into a weekly one should fail")
	}
}

func over(raw float64) []policy.Signal {
	return []policy.Signal{{Metric: "rps", Raw: raw, Value: raw, Target: 50}}
}

func dec(at time.Time, from, to int) Decision {
	return Decision{ID: NewID("web", at), At: at, Service: "web", From: from, To: to}
}

func run(e *Evaluator, from time.Time, ticks int, sig []policy.Signal) []Outcome {
	var out []Outcome
	for i := 1; i <= ticks; i++ {
		out = append(out, e.Observe(from.Add(time.Duration(i)*10*time.Second), sig)...)
	}
	return out
}

func TestEvaluatorVerdicts(t *testing.T) {
	cases := map[string]struct {
		from, to int
		load     float64
		reverse  bool
		want     string
	}{
		"scale-up that relieved load":  {2, 4, 45, false, VerdictEffective},
		"scale-up still overloaded":    {2, 3, 80, false, VerdictInsufficient},
		"scale-up reversed":            {2, 4, 20, true, VerdictUnnecessary},
		"scale-down that held":         {4, 3, 40, false, VerdictOK},
		"scale-down then overload":     {4, 3, 70, false, VerdictPremature},
		"scale-down reversed by scale": {4, 3, 40, true, VerdictPremature},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := &Evaluator{Window: time.Minute, Tolerance: 0.1}
			e.Track(dec(day0, tc.from, tc.to))
			if tc.reverse {
				e.Track(dec(day0.Add(20*time.Second), tc.to, tc.from))
			}
			outs := run(e, day0, 6, over(tc.load))
			if len(outs) == 0 || outs[0].Verdict != tc.want {
				t.Fatalf("outcomes = %+v, want first verdict %q", outs, tc.want)
			}
		})
	}
}

func TestEvaluatorOnlyCountsUndoneDecisionsAsReversed(t *testing.T) {
	e := &Evaluator{Window: time.Minute, Tolerance: 0.1}
	e.Track(dec(day0, 2, 3))
	e.Track(dec(day0.Add(10*time.Second), 3, 5))
	e.Track(dec(day0.Add(20*time.Second), 5, 4)) // trims, but stays above both starting points
	for _, o := range run(e, day0, 6, over(40)) {
		if o.Up && o.Reversed {
			t.Fatalf("scale-up %s marked reversed by a partial scale-down", o.DecisionID)
		}
	}
}

func TestEvaluatorIgnoresHistorySignalsAndDryRun(t *testing.T) {
	e := &Evaluator{Window: time.Minute, Tolerance: 0.1}
	e.Track(Decision{ID: "dry", At: day0, From: 1, To: 5, DryRun: true})
	if e.Pending() != 0 {
		t.Fatal("dry-run decisions must not be graded")
	}
	e.Track(dec(day0, 2, 4))
	sig := []policy.Signal{
		{Metric: "rps", Raw: 40, Target: 50},
		{Metric: "rps" + HistorySuffix, Raw: 999, Target: 50}, // forecast, not load
	}
	outs := run(e, day0, 6, sig)
	if len(outs) != 1 || outs[0].Verdict != VerdictEffective || outs[0].PeakRatio != 0.8 {
		t.Fatalf("outcomes = %+v", outs)
	}
}

func TestStoreJournalAndState(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := dec(day0, 2, 4)
	o := Outcome{DecisionID: d.ID, Verdict: VerdictEffective}
	if err := s.Append(Record{Type: "decision", Decision: &d}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Record{Type: "outcome", Outcome: &o}); err != nil {
		t.Fatal(err)
	}
	type state struct{ N int }
	if err := s.SaveState("web/a b", state{N: 7}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Simulate a crash mid-write: a torn final line must be skipped.
	f, _ := os.OpenFile(filepath.Join(dir, journalFile), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"decision","decis`)
	f.Close()

	recs, err := ReadJournal(dir)
	if err != nil || len(recs) != 2 {
		t.Fatalf("records = %d, %v", len(recs), err)
	}
	entries := Join(recs, "web", time.Time{})
	if len(entries) != 1 || entries[0].Outcome == nil || entries[0].Outcome.Verdict != VerdictEffective {
		t.Fatalf("entries = %+v", entries)
	}

	s2, _ := Open(dir)
	defer s2.Close()
	var got state
	if found, err := s2.LoadState("web/a b", &got); !found || err != nil || got.N != 7 {
		t.Fatalf("state = %+v, %v, %v", got, found, err)
	}
	if found, _ := s2.LoadState("missing", &got); found {
		t.Fatal("missing state reported as found")
	}
	if _, err := os.Stat(filepath.Join(dir, "state-web_a_b.json")); err != nil {
		t.Fatalf("service name not sanitized into the file name: %v", err)
	}
}

func TestAnalyzeAdvice(t *testing.T) {
	svc := config.Service{Name: "web", Behavior: config.Behavior{
		ScaleDownStabilization: 5 * time.Minute, ScaleDownCooldown: 2 * time.Minute, MaxStepUp: 4, ScaleUpCooldown: 30 * time.Second, Tolerance: 0.1,
	}}
	var entries []Entry
	for i := range 10 {
		at := day0.Add(time.Duration(i) * time.Hour)
		verdict := VerdictOK
		if i < 5 {
			verdict = VerdictPremature
		}
		entries = append(entries, Entry{Decision: dec(at, 4, 3), Outcome: &Outcome{Verdict: verdict}})
	}
	r := Analyze(entries, svc, day0, day0.Add(10*time.Hour))
	if r.Decisions != 10 || r.Verdicts[VerdictPremature] != 5 {
		t.Fatalf("report = %+v", r)
	}
	if len(r.Advice) == 0 || !strings.Contains(r.Advice[0], "scale_down_stabilization (now 5m0s)") {
		t.Fatalf("advice = %v", r.Advice)
	}

	few := Analyze(entries[:2], svc, day0, day0.Add(time.Hour))
	if !strings.Contains(strings.Join(few.Advice, " "), "not enough history") {
		t.Fatalf("advice with little history = %v", few.Advice)
	}
}
