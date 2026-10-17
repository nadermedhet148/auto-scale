package metrics

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const (
	requestsTotal   = "traefik_service_requests_total"
	durationBuckets = "traefik_service_request_duration_seconds_bucket"
)

// Traefik derives request rate and p95 latency for one Traefik service from
// the difference between consecutive scrapes of its cumulative counters.
type Traefik struct {
	url, service string
	client       *http.Client
	now          func() time.Time

	have        bool
	prevAt      time.Time
	prevTotal   float64
	prevBuckets map[float64]float64
}

type TraefikSample struct {
	RPS   float64 // total across all replicas
	P95ms float64
	RPSOK bool
	P95OK bool // false when there was no traffic in the interval
}

func NewTraefik(url, service string) *Traefik {
	return &Traefik{
		url:     url,
		service: service,
		client:  &http.Client{Timeout: 5 * time.Second},
		now:     time.Now,
	}
}

func (t *Traefik) Scrape(ctx context.Context) (TraefikSample, error) {
	var out TraefikSample
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url, nil)
	if err != nil {
		return out, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("scrape %s: %s", t.url, resp.Status)
	}
	samples, err := Parse(resp.Body)
	if err != nil {
		return out, err
	}
	now := t.now()

	var total float64
	buckets := map[float64]float64{}
	for _, s := range samples {
		if s.Labels["service"] != t.service {
			continue
		}
		switch s.Name {
		case requestsTotal:
			total += s.Value
		case durationBuckets:
			if le, err := strconv.ParseFloat(s.Labels["le"], 64); err == nil {
				buckets[le] += s.Value
			}
		}
	}

	if t.have {
		if dt := now.Sub(t.prevAt).Seconds(); dt > 0 {
			out.RPS, out.RPSOK = counterDelta(total, t.prevTotal)/dt, true
		}
		delta := make(map[float64]float64, len(buckets))
		for le, v := range buckets {
			delta[le] = counterDelta(v, t.prevBuckets[le])
		}
		if q, ok := Quantile(0.95, delta); ok {
			out.P95ms, out.P95OK = q*1000, true
		}
	}
	t.have, t.prevAt, t.prevTotal, t.prevBuckets = true, now, total, buckets
	return out, nil
}

// counterDelta treats a decrease as a counter reset (e.g. Traefik restarted).
func counterDelta(cur, prev float64) float64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}
