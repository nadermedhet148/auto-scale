package model

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Unix(0, 0)

func TestEWMA(t *testing.T) {
	e := &EWMA{Alpha: 0.5}
	for i, tc := range []struct{ in, want float64 }{{10, 10}, {20, 15}, {20, 17.5}} {
		if got := e.Update(t0, tc.in); math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("step %d: got %v, want %v", i, got, tc.want)
		}
	}
}

func TestHoltForecastsRisingTrend(t *testing.T) {
	h := &Holt{Alpha: 0.8, Beta: 0.8, Horizon: 1}
	var f float64
	for _, x := range []float64{10, 20, 30, 40, 50} {
		f = h.Update(t0, x)
	}
	if f <= 50 {
		t.Fatalf("forecast %v should lead the last sample (50)", f)
	}
}

func TestHoltNeverNegative(t *testing.T) {
	h := &Holt{Alpha: 0.8, Beta: 0.8, Horizon: 5}
	for _, x := range []float64{50, 30, 10, 0, 0} {
		if f := h.Update(t0, x); f < 0 {
			t.Fatalf("forecast went negative: %v", f)
		}
	}
}

func TestHoltWintersLearnsSeason(t *testing.T) {
	// A square wave with an 80s cycle sampled every 10s: 4 ticks at 10, 4 at 50.
	wave := func(i int) float64 {
		if i%8 < 4 {
			return 10
		}
		return 50
	}
	at := func(i int) time.Time { return time.Unix(int64(i)*10, 0) }
	hw := &HoltWinters{Alpha: 0.3, Beta: 0.1, Gamma: 0.3, SeasonLength: 80 * time.Second, Interval: 10 * time.Second, Horizon: 1}
	i := 0
	for ; i < 8*6; i++ {
		hw.Update(at(i), wave(i))
	}
	// Feeding the last low tick of a cycle, the 1-step forecast should
	// anticipate the jump to the high phase.
	for wave(i) != 10 || wave(i+1) != 50 {
		hw.Update(at(i), wave(i))
		i++
	}
	if f := hw.Update(at(i), wave(i)); f < 40 {
		t.Fatalf("forecast %v did not anticipate the seasonal rise to 50", f)
	}
}

func TestHoltWintersSeasonSurvivesGap(t *testing.T) {
	wave := func(i int) float64 { return 30 + 20*math.Sin(2*math.Pi*float64(i%8)/8) }
	at := func(i int) time.Time { return time.Unix(int64(i)*10, 0) }
	opts := Options{Kind: "holt_winters", Alpha: 0.3, Beta: 0.1, Gamma: 0.3, SeasonLength: 80 * time.Second, Interval: 10 * time.Second, Horizon: 1}
	p := NewPipeline(opts)
	for i := range 8 * 6 {
		p.Update(at(i), wave(i))
	}
	saved := p.State()

	// Restart after a gap of 1003 ticks; slots are aligned to the wall clock,
	// so the learned season still lines up with the data.
	q := NewPipeline(opts)
	if err := q.Restore(saved, false); err != nil {
		t.Fatal(err)
	}
	var worst float64
	for i := 1051; i < 1051+8; i++ {
		f := q.Update(at(i), wave(i))
		worst = math.Max(worst, math.Abs(f-wave(i+1)))
	}
	if worst > 8 {
		t.Fatalf("after restore the 1-step forecast was off by %.1f; season misaligned", worst)
	}
}

func TestRestoreRejectsChangedModel(t *testing.T) {
	ewma := NewPipeline(Options{Kind: "ewma", Alpha: 0.5})
	holt := NewPipeline(Options{Kind: "holt", Alpha: 0.5, Beta: 0.3})
	if err := holt.Restore(ewma.State(), true); err == nil {
		t.Fatal("restoring ewma state into holt should fail")
	}
}

func TestRestoreFreshVersusStale(t *testing.T) {
	src := NewPipeline(Options{Kind: "ewma", Alpha: 0.5})
	src.Update(t0, 100)
	fresh := NewPipeline(Options{Kind: "ewma", Alpha: 0.5})
	_ = fresh.Restore(src.State(), true)
	if got := fresh.Update(t0, 0); got != 50 {
		t.Fatalf("fresh restore: got %v, want 50", got)
	}
	stale := NewPipeline(Options{Kind: "ewma", Alpha: 0.5})
	_ = stale.Restore(src.State(), false)
	if got := stale.Update(t0, 0); got != 0 {
		t.Fatalf("stale restore should start over: got %v, want 0", got)
	}
}

func TestOutlierFilterClampsSpike(t *testing.T) {
	f := &OutlierFilter{K: 3.5, Size: 12}
	for _, x := range []float64{10, 11, 10, 12, 11, 10} {
		f.Apply(x)
	}
	if got := f.Apply(100); got >= 20 || got <= 10.5 {
		t.Fatalf("spike not clamped near the median: %v", got)
	}
}

func TestOutlierFilterAcceptsSustainedShift(t *testing.T) {
	f := &OutlierFilter{K: 3.5, Size: 6}
	for _, x := range []float64{10, 11, 10, 12, 11, 10} {
		f.Apply(x)
	}
	for i := 0; i < 6; i++ {
		if f.Apply(100) == 100 {
			return
		}
	}
	t.Fatal("sustained shift never passed through the filter")
}
