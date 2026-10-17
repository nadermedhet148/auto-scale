package metrics

import (
	"math"
	"strings"
	"testing"
)

// FuzzParse checks the parser never panics and that whatever it accepts
// has a metric name. Run longer with: go test -fuzz=FuzzParse ./internal/metrics
func FuzzParse(f *testing.F) {
	f.Add(traefikExposition(1))
	f.Add(`m{a="x\"}",b="\\"} 1 1700000000`)
	f.Add(`m{a="unterminated} 1`)
	f.Add("m +Inf\nm NaN\n# comment\n\n")
	f.Add(`{a="b"} 1`)
	f.Fuzz(func(t *testing.T, in string) {
		samples, _ := Parse(strings.NewReader(in))
		for _, s := range samples {
			if s.Name == "" || strings.ContainsAny(s.Name, "{ ") {
				t.Fatalf("bad name %q from %q", s.Name, in)
			}
		}
	})
}

// FuzzQuantile checks estimates stay within the finite bucket bounds for any
// cumulative histogram.
func FuzzQuantile(f *testing.F) {
	f.Add(10.0, 20.0, 30.0, 0.95)
	f.Add(0.0, 0.0, 5.0, 0.5)
	f.Fuzz(func(t *testing.T, a, b, c, q float64) {
		if math.IsNaN(a+b+c+q) || a < 0 || b < 0 || c < 0 || q < 0 || q > 1 {
			t.Skip()
		}
		// Make counts cumulative: a <= a+b <= a+b+c.
		buckets := map[float64]float64{0.1: a, 0.5: a + b, math.Inf(1): a + b + c}
		v, ok := Quantile(q, buckets)
		if !ok {
			return
		}
		if v < 0 || v > 0.5 || math.IsNaN(v) {
			t.Fatalf("quantile(%v) = %v outside [0, 0.5] for %v", q, v, buckets)
		}
	})
}
