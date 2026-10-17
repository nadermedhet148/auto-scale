// Package controller runs the control loop: observe replicas, collect
// metrics, estimate, decide, act.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"dockscale/internal/config"
	"dockscale/internal/docker"
	"dockscale/internal/engine"
	"dockscale/internal/metrics"
	"dockscale/internal/policy"
	"dockscale/internal/queue"
	"dockscale/internal/replay"
)

// applyTimeout bounds one scaling action, which may include an image pull.
const applyTimeout = 3 * time.Minute

// Docker is the subset of the Docker client the controller needs.
type Docker interface {
	Replicas(ctx context.Context, service string) (live, dead []docker.Replica, err error)
	Start(ctx context.Context, svc config.Service) (string, error)
	Remove(ctx context.Context, id string, grace time.Duration) error
	Usage(ctx context.Context, ids []string, cpuLimit float64) docker.Usage
}

type Controller struct {
	cfg  *config.Config
	dk   Docker
	now  func() time.Time
	log  *slog.Logger
	rec  *replay.Recorder // nil unless recording
	svcs []*service

	mu sync.RWMutex // guards service.status
}

type service struct {
	cfg     config.Service
	engine  *engine.Engine
	traefik *metrics.Traefik
	queue   queue.Source
	status  Status
}

// Status is the last observed state of a service, served on /status.
type Status struct {
	Service     string          `json:"service"`
	Replicas    int             `json:"replicas"`
	Recommended int             `json:"recommended"`
	Desired     int             `json:"desired"`
	Signals     []policy.Signal `json:"signals"`
	Reason      string          `json:"reason"`
	LastScale   time.Time       `json:"last_scale,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// New builds a controller. rec may be nil; when set, every tick's load is
// appended to it for later replay.
func New(cfg *config.Config, dk Docker, rec *replay.Recorder, log *slog.Logger) (*Controller, error) {
	c := &Controller{cfg: cfg, dk: dk, now: time.Now, rec: rec, log: log}
	for _, sc := range cfg.Services {
		s := &service{cfg: sc, engine: engine.New(sc), status: Status{Service: sc.Name}}
		if sc.UsesTraefik() {
			s.traefik = metrics.NewTraefik(sc.Prometheus.URL, sc.Prometheus.Service)
		}
		if sc.UsesQueue() {
			q, err := queue.New(*sc.Queue)
			if err != nil {
				return nil, fmt.Errorf("service %q: %w", sc.Name, err)
			}
			s.queue = q
		}
		c.svcs = append(c.svcs, s)
	}
	return c, nil
}

func (c *Controller) Run(ctx context.Context) {
	c.tickAll(ctx)
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tickAll(ctx)
		}
	}
}

func (c *Controller) tickAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range c.svcs {
		wg.Add(1)
		go func(s *service) {
			defer wg.Done()
			c.tick(ctx, s)
		}(s)
	}
	wg.Wait()
}

func (c *Controller) tick(ctx context.Context, s *service) {
	log := c.log.With("service", s.cfg.Name)
	now := c.now()

	c.mu.RLock()
	st := Status{Service: s.cfg.Name, UpdatedAt: now, LastScale: s.status.LastScale}
	c.mu.RUnlock()
	defer func() {
		c.mu.Lock()
		s.status = st
		c.mu.Unlock()
	}()

	obsCtx, cancel := context.WithTimeout(ctx, c.cfg.Interval)
	defer cancel()

	live, dead, err := c.dk.Replicas(obsCtx, s.cfg.Name)
	if err != nil {
		st.LastError = err.Error()
		log.Error("list replicas", "err", err)
		return
	}
	for _, r := range dead {
		if err := c.dk.Remove(obsCtx, r.ID, 0); err != nil {
			log.Warn("clean up stopped replica", "name", r.Name, "err", err)
		}
	}
	current := len(live)
	st.Replicas = current

	perReplica := c.collect(obsCtx, s, live, log)
	if c.rec != nil {
		row := replay.Row{At: now, Service: s.cfg.Name, Replicas: current, Totals: engine.Totals(perReplica, current)}
		if err := c.rec.Record(row); err != nil {
			log.Warn("record", "err", err)
		}
	}

	signals, d := s.engine.Step(now, current, perReplica)
	st.Signals, st.Recommended, st.Desired, st.Reason = signals, d.Recommended, d.Desired, d.Reason

	if d.Desired == current {
		log.Debug("hold", "replicas", current, "reason", d.Reason)
		return
	}
	log.Info("scale", "from", current, "to", d.Desired, "reason", d.Reason, "dry_run", c.cfg.DryRun)
	if c.cfg.DryRun {
		s.engine.Commit(now, current, d.Desired)
		st.LastScale = now
		return
	}

	actCtx, cancelAct := context.WithTimeout(ctx, applyTimeout)
	defer cancelAct()
	n, err := c.apply(actCtx, s, live, d.Desired)
	if n != current {
		s.engine.Commit(now, current, n)
		st.LastScale, st.Replicas = now, n
	}
	if err != nil {
		st.LastError = err.Error()
		log.Error("scale", "reached", n, "wanted", d.Desired, "err", err)
	}
}

// collect returns this tick's per-replica metric values. Metrics that could
// not be read are absent, and the engine skips them.
func (c *Controller) collect(ctx context.Context, s *service, live []docker.Replica, log *slog.Logger) map[string]float64 {
	out := map[string]float64{}

	if s.cfg.UsesDockerStats() && len(live) > 0 {
		ids := make([]string, len(live))
		for i, r := range live {
			ids[i] = r.ID
		}
		u := c.dk.Usage(ctx, ids, s.cfg.Template.CPUs)
		if u.Errors > 0 {
			log.Warn("stats unavailable for some replicas", "failed", u.Errors, "of", len(ids))
		}
		if u.CPUN > 0 {
			out[config.MetricCPU] = u.CPU
		}
		if u.MemN > 0 {
			out[config.MetricMemory] = u.Mem
		}
	}

	if s.traefik != nil {
		ts, err := s.traefik.Scrape(ctx)
		if err != nil {
			log.Warn("scrape traefik", "err", err)
		} else {
			if ts.RPSOK && len(live) > 0 {
				out[config.MetricRPS] = ts.RPS / float64(len(live))
			}
			if ts.P95OK {
				out[config.MetricLatencyP95] = ts.P95ms
			}
		}
	}

	if s.queue != nil && len(live) > 0 {
		depth, err := s.queue.Depth(ctx)
		if err != nil {
			log.Warn("read queue depth", "err", err)
		} else {
			out[config.MetricQueueDepth] = depth / float64(len(live))
		}
	}
	return out
}

// apply starts or removes replicas in parallel and returns how many are
// running afterwards. Scale-down removes the newest replicas first.
func (c *Controller) apply(ctx context.Context, s *service, live []docker.Replica, desired int) (int, error) {
	current := len(live)
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []error
		done int
	)
	record := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs = append(errs, err)
		} else {
			done++
		}
	}

	if desired > current {
		for i := 0; i < desired-current; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := c.dk.Start(ctx, s.cfg)
				record(err)
			}()
		}
		wg.Wait()
		return current + done, errors.Join(errs...)
	}

	for _, r := range live[desired:] {
		wg.Add(1)
		go func(r docker.Replica) {
			defer wg.Done()
			record(c.dk.Remove(ctx, r.ID, s.cfg.Template.StopTimeout))
		}(r)
	}
	wg.Wait()
	return current - done, errors.Join(errs...)
}

func (c *Controller) Statuses() []Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Status, len(c.svcs))
	for i, s := range c.svcs {
		out[i] = s.status
	}
	return out
}
