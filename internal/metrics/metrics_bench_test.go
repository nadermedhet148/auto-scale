package metrics

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// traefikExposition builds a realistic scrape: services × status codes ×
// (one request counter + 12 latency buckets).
func traefikExposition(services int) string {
	les := []string{"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10", "+Inf"}
	var b strings.Builder
	b.WriteString("# HELP traefik_service_requests_total How many HTTP requests processed.\n")
	b.WriteString("# TYPE traefik_service_requests_total counter\n")
	for s := range services {
		for _, code := range []string{"200", "301", "404", "500"} {
			lbl := fmt.Sprintf(`code="%s",method="GET",protocol="http",service="svc%d@docker"`, code, s)
			fmt.Fprintf(&b, "traefik_service_requests_total{%s} %d\n", lbl, 1000*(s+1))
			for i, le := range les {
				fmt.Fprintf(&b, "traefik_service_request_duration_seconds_bucket{%s,le=\"%s\"} %d\n", lbl, le, 80*(i+1))
			}
		}
	}
	return b.String()
}

func BenchmarkParse(b *testing.B) {
	for _, n := range []int{1, 20, 200} {
		body := traefikExposition(n)
		b.Run(fmt.Sprintf("services=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Parse(strings.NewReader(body)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkQuantile(b *testing.B) {
	buckets := map[float64]float64{}
	for i, le := range []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10} {
		buckets[le] = float64(80 * (i + 1))
	}
	buckets[math.Inf(1)] = 960
	b.ReportAllocs()
	for b.Loop() {
		Quantile(0.95, buckets)
	}
}
