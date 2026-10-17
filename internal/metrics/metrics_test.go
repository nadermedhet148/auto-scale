package metrics

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const exposition = `# HELP traefik_service_requests_total How many HTTP requests processed.
# TYPE traefik_service_requests_total counter
traefik_service_requests_total{code="200",method="GET",protocol="http",service="web@docker"} %d
traefik_service_requests_total{code="500",method="GET",protocol="http",service="web@docker"} 0
traefik_service_requests_total{code="200",method="GET",protocol="http",service="other@docker"} 999
traefik_service_request_duration_seconds_bucket{code="200",method="GET",protocol="http",service="web@docker",le="0.1"} %d
traefik_service_request_duration_seconds_bucket{code="200",method="GET",protocol="http",service="web@docker",le="0.3"} %d
traefik_service_request_duration_seconds_bucket{code="200",method="GET",protocol="http",service="web@docker",le="1.2"} %d
traefik_service_request_duration_seconds_bucket{code="200",method="GET",protocol="http",service="web@docker",le="+Inf"} %d
`

func TestParse(t *testing.T) {
	samples, err := Parse(strings.NewReader(fmt.Sprintf(exposition, 100, 50, 90, 100, 100)))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 7 {
		t.Fatalf("got %d samples, want 7", len(samples))
	}
	if s := samples[0]; s.Labels["service"] != "web@docker" || s.Value != 100 {
		t.Fatalf("bad first sample: %+v", s)
	}
	if le := samples[6].Labels["le"]; le != "+Inf" {
		t.Fatalf("le label = %q", le)
	}
}

func TestParseQuotedBrace(t *testing.T) {
	samples, _ := Parse(strings.NewReader(`m{path="/a}b",x="q\"y"} 3 1700000000`))
	if len(samples) != 1 || samples[0].Labels["path"] != "/a}b" || samples[0].Labels["x"] != `q"y` || samples[0].Value != 3 {
		t.Fatalf("got %+v", samples)
	}
}

func TestQuantile(t *testing.T) {
	b := map[float64]float64{0.1: 50, 0.3: 90, 1.2: 100, math.Inf(1): 100}
	if q, _ := Quantile(0.95, b); math.Abs(q-0.75) > 1e-9 {
		t.Fatalf("p95 = %v, want 0.75", q)
	}
	if q, _ := Quantile(0.5, b); math.Abs(q-0.1) > 1e-9 {
		t.Fatalf("p50 = %v, want 0.1", q)
	}
	if _, ok := Quantile(0.95, map[float64]float64{math.Inf(1): 0}); ok {
		t.Fatal("empty histogram should not yield a quantile")
	}
}

func TestTraefikScrape(t *testing.T) {
	bodies := []string{
		fmt.Sprintf(exposition, 100, 50, 90, 100, 100),
		fmt.Sprintf(exposition, 300, 150, 270, 300, 300), // +200 requests
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, bodies[calls])
		calls++
	}))
	defer srv.Close()

	clock := time.Unix(0, 0)
	tr := NewTraefik(srv.URL, "web@docker")
	tr.now = func() time.Time { return clock }

	first, err := tr.Scrape(context.Background())
	if err != nil || first.RPSOK {
		t.Fatalf("first scrape should only set a baseline: %+v %v", first, err)
	}
	clock = clock.Add(10 * time.Second)
	s, err := tr.Scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.RPSOK || math.Abs(s.RPS-20) > 1e-9 {
		t.Fatalf("rps = %v, want 20", s.RPS)
	}
	if !s.P95OK || math.Abs(s.P95ms-750) > 1e-6 {
		t.Fatalf("p95 = %vms, want 750", s.P95ms)
	}
}
