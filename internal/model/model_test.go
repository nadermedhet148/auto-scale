package model

import (
	"math"
	"testing"
)

func TestEWMA(t *testing.T) {
	e := &EWMA{Alpha: 0.5}
	for i, tc := range []struct{ in, want float64 }{{10, 10}, {20, 15}, {20, 17.5}} {
		if got := e.Update(tc.in); math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("step %d: got %v, want %v", i, got, tc.want)
		}
	}
}

func TestHoltForecastsRisingTrend(t *testing.T) {
	h := &Holt{Alpha: 0.8, Beta: 0.8, Horizon: 1}
	var f float64
	for _, x := range []float64{10, 20, 30, 40, 50} {
		f = h.Update(x)
	}
	if f <= 50 {
		t.Fatalf("forecast %v should lead the last sample (50)", f)
	}
}

func TestHoltNeverNegative(t *testing.T) {
	h := &Holt{Alpha: 0.8, Beta: 0.8, Horizon: 5}
	for _, x := range []float64{50, 30, 10, 0, 0} {
		if f := h.Update(x); f < 0 {
			t.Fatalf("forecast went negative: %v", f)
		}
	}
}

func TestHoltWintersLearnsSeason(t *testing.T) {
	// A square wave: 4 ticks at 10, 4 ticks at 50.
	wave := func(i int) float64 {
		if i%8 < 4 {
			return 10
		}
		return 50
	}
	hw := &HoltWinters{Alpha: 0.3, Beta: 0.1, Gamma: 0.3, Period: 8, Horizon: 1}
	var i int
	for ; i < 8*6; i++ {
		hw.Update(wave(i))
	}
	// Feeding the last low tick of a cycle, the 1-step forecast should
	// anticipate the jump to the high phase.
	for wave(i) != 10 || wave(i+1) != 50 {
		hw.Update(wave(i))
		i++
	}
	if f := hw.Update(wave(i)); f < 40 {
		t.Fatalf("forecast %v did not anticipate the seasonal rise to 50", f)
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
