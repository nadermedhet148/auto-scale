// Command dockscale-demo runs the real controller, web UI and memory against
// an in-memory fake Docker with synthetic load, so the dashboard can be tried
// without Docker. It fast-forwards a few simulated days first, so history,
// grades and the load profile are already populated, then keeps ticking at
// -speed times real time.
//
//	go run ./cmd/dockscale-demo            # then open http://localhost:9090
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"dockscale/internal/config"
	"dockscale/internal/controller"
	"dockscale/internal/docker"
	"dockscale/internal/memory"
)

// Two services with different daily shapes: an API with business-hours
// traffic (09:00-17:00 UTC) and a worker with a nightly batch (01:00-03:00 UTC).
const demoConfig = `
interval: 10s
dry_run: false
memory:
  dir: PLACEHOLDER
  snapshot_every: 30m   # simulated time; keeps the fast-forward from writing thousands of snapshots
services:
  - name: api
    min: 2
    max: 15
    template: {image: demo/api, cpus: 1}
    metrics:
      - {type: cpu, target: 60}
      - {type: memory, target: 85}
    model: {kind: holt, alpha: 0.5, beta: 0.3, horizon: 3}
    history: {prewarm: true}
  - name: worker
    min: 1
    max: 10
    template: {image: demo/worker, cpus: 2}
    metrics:
      - {type: cpu, target: 70}
    model: {kind: ewma, alpha: 0.4}
    behavior: {scale_down_stabilization: 10m}
`

func main() {
	listen := flag.String("listen", ":9090", "HTTP address for the UI")
	days := flag.Int("days", 3, "simulated days to fast-forward before going live")
	speed := flag.Float64("speed", 10, "simulated seconds per real second once live")
	memDir := flag.String("memory-dir", "", "memory directory (default: a fresh temp dir)")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := run(*listen, *days, *speed, *memDir, log); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(listen string, days int, speed float64, memDir string, log *slog.Logger) error {
	if memDir == "" {
		d, err := os.MkdirTemp("", "dockscale-demo-")
		if err != nil {
			return err
		}
		memDir = d
	}
	cfg, err := config.Parse([]byte(replaceDir(demoConfig, memDir)))
	if err != nil {
		return err
	}
	store, err := memory.Open(memDir)
	if err != nil {
		return err
	}
	defer store.Close()

	// The simulated clock starts `days` ago, so after the fast-forward the
	// simulated time of day matches the real one.
	clock := &simClock{t: time.Now().Add(-time.Duration(days) * 24 * time.Hour).Truncate(cfg.Interval)}
	dk := newFakeDocker(clock)
	ctl, err := controller.New(cfg, dk, nil, store, log)
	if err != nil {
		return err
	}
	ctl.SetClock(clock.Now)
	ctl.SetBanner("demo")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("fast-forwarding %d simulated days...\n", days)
	start := time.Now()
	for clock.Now().Before(time.Now()) && ctx.Err() == nil {
		ctl.Tick(ctx)
		clock.Advance(cfg.Interval)
	}
	fmt.Printf("done in %v. memory in %s\n", time.Since(start).Round(time.Millisecond), memDir)

	srv := &http.Server{Addr: listen, Handler: ctl.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "http:", err)
			stop()
		}
	}()
	url := "http://" + listen
	if strings.HasPrefix(listen, ":") {
		url = "http://localhost" + listen
	}
	fmt.Printf("dashboard: %s  (one tick every %v real time; Ctrl+C to stop)\n",
		url, time.Duration(float64(cfg.Interval)/speed).Round(time.Millisecond))

	t := time.NewTicker(time.Duration(float64(cfg.Interval) / speed))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			ctl.Close()
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return srv.Shutdown(sctx)
		case <-t.C:
			clock.Advance(cfg.Interval)
			ctl.Tick(ctx)
		}
	}
}

func replaceDir(cfg, dir string) string {
	// Quote the path: Windows temp dirs contain backslashes and colons.
	return strings.Replace(cfg, "PLACEHOLDER", fmt.Sprintf("%q", dir), 1)
}

type simClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *simClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *simClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeDocker simulates containers. A replica takes load 30s after it starts.
// CPU is the service's total demand divided across ready replicas.
type fakeDocker struct {
	mu    sync.Mutex
	clock *simClock
	rng   *rand.Rand
	live  map[string][]replica // service -> replicas
	byID  map[string]string    // id -> service
	next  int
	spike map[string]int // ticks of spike remaining per service
}

type replica struct {
	docker.Replica
	started time.Time
}

const startupDelay = 30 * time.Second

func newFakeDocker(c *simClock) *fakeDocker {
	return &fakeDocker{clock: c, rng: rand.New(rand.NewPCG(7, 11)), live: map[string][]replica{},
		byID: map[string]string{}, spike: map[string]int{}}
}

func (f *fakeDocker) Replicas(_ context.Context, service string) ([]docker.Replica, []docker.Replica, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rs := f.live[service]
	sort.Slice(rs, func(i, j int) bool { return rs[i].Created < rs[j].Created })
	out := make([]docker.Replica, len(rs))
	for i, r := range rs {
		out[i] = r.Replica
	}
	return out, nil, nil
}

func (f *fakeDocker) Start(_ context.Context, svc config.Service) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	now := f.clock.Now()
	id := fmt.Sprintf("%s-%06d", svc.Name, f.next)
	f.live[svc.Name] = append(f.live[svc.Name], replica{
		Replica: docker.Replica{ID: id, Name: id, Created: now.UnixNano()}, started: now,
	})
	f.byID[id] = svc.Name
	return id, nil
}

func (f *fakeDocker) Remove(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	svc := f.byID[id]
	rs := f.live[svc]
	for i, r := range rs {
		if r.ID == id {
			f.live[svc] = append(rs[:i], rs[i+1:]...)
			break
		}
	}
	delete(f.byID, id)
	return nil
}

func (f *fakeDocker) Usage(_ context.Context, ids []string, _ float64) docker.Usage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(ids) == 0 {
		return docker.Usage{}
	}
	svc := f.byID[ids[0]]
	now := f.clock.Now()
	ready := 0
	for _, r := range f.live[svc] {
		if now.Sub(r.started) >= startupDelay {
			ready++
		}
	}
	u := docker.Usage{Mem: 35 + 4*f.rng.NormFloat64(), MemN: len(ids)}
	if ready > 0 {
		u.CPU, u.CPUN = f.demand(svc, now)/float64(ready), ready
	}
	return u
}

// demand is the service's total CPU demand in % of one replica's limit.
func (f *fakeDocker) demand(svc string, t time.Time) float64 {
	t = t.UTC()
	hour := float64(t.Hour()) + float64(t.Minute())/60
	var base float64
	switch svc {
	case "api":
		base = 60 // overnight
		if hour >= 9 && hour < 17 {
			base = 520 + 80*math.Sin(2*math.Pi*(hour-9)/8)
		}
	case "worker":
		base = 50
		if hour >= 1 && hour < 3 {
			base = 480
		}
	}
	if f.spike[svc] == 0 && f.rng.Float64() < 0.003 {
		f.spike[svc] = 4
	}
	if f.spike[svc] > 0 {
		f.spike[svc]--
		base *= 2
	}
	return math.Max(0, base*(1+0.07*f.rng.NormFloat64()))
}
