// Package controller runs the control loop: observe replicas, collect
// metrics, estimate, decide, act.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"dockscale/internal/config"
	"dockscale/internal/docker"
	"dockscale/internal/engine"
	"dockscale/internal/memory"
	"dockscale/internal/metrics"
	"dockscale/internal/policy"
	"dockscale/internal/queue"
	"dockscale/internal/replay"
)

// applyTimeout bounds one scaling action, which may include an image pull.
const applyTimeout = 3 * time.Minute

// recentLimit is how many decisions per service /history keeps in memory.
const recentLimit = 200

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
	mem  *memory.Store    // nil unless memory.dir is set
	svcs []*service

	mu sync.RWMutex // guards service.status and service.recent
}

type service struct {
	cfg     config.Service
	engine  *engine.Engine
	traefik *metrics.Traefik
	queue   queue.Source
	eval    *memory.Evaluator
	status  Status
	recent  []memory.Entry // newest last
	saved   time.Time      // last snapshot
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

// New builds a controller. rec and mem may be nil. With rec, every tick's
// load is appended for later replay. With mem, learned state is restored at
// startup and saved periodically, and decisions are journalled with their
// outcomes.
func New(cfg *config.Config, dk Docker, rec *replay.Recorder, mem *memory.Store, log *slog.Logger) (*Controller, error) {
	c := &Controller{cfg: cfg, dk: dk, now: time.Now, rec: rec, mem: mem, log: log}
	var journal []memory.Record
	if mem != nil {
		var err error
		if journal, err = memory.ReadJournal(mem.Dir()); err != nil {
			return nil, fmt.Errorf("read journal: %w", err)
		}
	}
	for _, sc := range cfg.Services {
		s := &service{
			cfg: sc, engine: engine.New(sc, cfg.Interval), status: Status{Service: sc.Name},
			eval: &memory.Evaluator{Window: cfg.Memory.EvaluateAfter, Tolerance: sc.Behavior.Tolerance},
		}
		if mem != nil {
			c.restore(s, journal)
		}
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

func (c *Controller) restore(s *service, journal []memory.Record) {
	log := c.log.With("service", s.cfg.Name)
	var snap engine.Snapshot
	found, err := c.mem.LoadState(s.cfg.Name, &snap)
	switch {
	case err != nil:
		log.Warn("ignoring saved state", "err", err)
	case found:
		for _, note := range s.engine.Restore(snap, c.now(), c.cfg.Memory.MaxStateAge) {
			log.Info("restored memory", "detail", note)
		}
	}
	entries := memory.Join(journal, s.cfg.Name, time.Time{})
	if len(entries) > recentLimit {
		entries = entries[len(entries)-recentLimit:]
	}
	s.recent = entries
}

// Close saves every service's learned state. Call it on shutdown.
func (c *Controller) Close() {
	if c.mem == nil {
		return
	}
	now := c.now()
	for _, s := range c.svcs {
		c.snapshot(s, now)
	}
}

func (c *Controller) snapshot(s *service, now time.Time) {
	if err := c.mem.SaveState(s.cfg.Name, s.engine.Snapshot(now)); err != nil {
		c.log.Warn("save state", "service", s.cfg.Name, "err", err)
		return
	}
	s.saved = now
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
	c.grade(s, now, signals)
	if c.mem != nil && now.Sub(s.saved) >= c.cfg.Memory.SnapshotEvery {
		c.snapshot(s, now)
	}

	if d.Desired == current {
		log.Debug("hold", "replicas", current, "reason", d.Reason)
		return
	}
	log.Info("scale", "from", current, "to", d.Desired, "reason", d.Reason, "dry_run", c.cfg.DryRun)
	dec := memory.Decision{
		ID: memory.NewID(s.cfg.Name, now), At: now, Service: s.cfg.Name,
		From: current, To: d.Desired, Wanted: d.Desired, Recommended: d.Recommended,
		Reason: d.Reason, Signals: signals, DryRun: c.cfg.DryRun,
	}
	if c.cfg.DryRun {
		s.engine.Commit(now, current, d.Desired)
		st.LastScale = now
		c.journal(s, dec)
		return
	}

	actCtx, cancelAct := context.WithTimeout(ctx, applyTimeout)
	defer cancelAct()
	n, err := c.apply(actCtx, s, live, d.Desired)
	if n != current {
		s.engine.Commit(now, current, n)
		st.LastScale, st.Replicas = now, n
	}
	dec.To = n
	if err != nil {
		st.LastError = err.Error()
		dec.Error = err.Error()
		log.Error("scale", "reached", n, "wanted", d.Desired, "err", err)
	}
	c.journal(s, dec)
}

// grade feeds this tick's signals to the outcome evaluator and records the
// verdicts of decisions whose evaluation window has ended.
func (c *Controller) grade(s *service, now time.Time, signals []policy.Signal) {
	for _, o := range s.eval.Observe(now, signals) {
		c.log.Info("decision outcome", "service", s.cfg.Name, "decision", o.DecisionID, "verdict", o.Verdict,
			"overload", fmt.Sprintf("%.0f%%", 100*o.OverloadFraction), "peak_ratio", fmt.Sprintf("%.2f", o.PeakRatio))
		if c.mem != nil {
			if err := c.mem.Append(memory.Record{Type: "outcome", Outcome: &o}); err != nil {
				c.log.Warn("journal outcome", "err", err)
			}
		}
		c.mu.Lock()
		for i := range s.recent {
			if s.recent[i].Decision.ID == o.DecisionID {
				s.recent[i].Outcome = &o
			}
		}
		c.mu.Unlock()
	}
}

// journal records a decision and starts watching its outcome.
func (c *Controller) journal(s *service, d memory.Decision) {
	s.eval.Track(d)
	if c.mem != nil {
		if err := c.mem.Append(memory.Record{Type: "decision", Decision: &d}); err != nil {
			c.log.Warn("journal decision", "err", err)
		}
	}
	c.mu.Lock()
	s.recent = append(s.recent, memory.Entry{Decision: d})
	if len(s.recent) > recentLimit {
		s.recent = s.recent[len(s.recent)-recentLimit:]
	}
	c.mu.Unlock()
}

// History returns recent decisions for a service ("" for all), newest first.
func (c *Controller) History(service string, limit int) []memory.Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []memory.Entry
	for _, s := range c.svcs {
		if service == "" || s.cfg.Name == service {
			out = append(out, s.recent...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Decision.At.After(out[j].Decision.At) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
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
