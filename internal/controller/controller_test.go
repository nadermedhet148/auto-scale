package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"dockscale/internal/config"
	"dockscale/internal/docker"
	"dockscale/internal/memory"
	"dockscale/internal/replay"
)

type fakeDocker struct {
	mu       sync.Mutex
	live     []docker.Replica
	dead     []docker.Replica
	next     int64
	startErr func(n int) error // n = 1-based start attempt
	starts   int
	removed  []string
	listErr  error
	usage    docker.Usage
}

func (f *fakeDocker) Replicas(context.Context, string) ([]docker.Replica, []docker.Replica, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, nil, f.listErr
	}
	return append([]docker.Replica(nil), f.live...), append([]docker.Replica(nil), f.dead...), nil
}

func (f *fakeDocker) Start(_ context.Context, svc config.Service) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.startErr != nil {
		if err := f.startErr(f.starts); err != nil {
			return "", err
		}
	}
	f.next++
	r := docker.Replica{ID: fmt.Sprintf("id%d", f.next), Name: fmt.Sprintf("%s-%d", svc.Name, f.next), Created: f.next}
	f.live = append(f.live, r)
	return r.ID, nil
}

func (f *fakeDocker) Remove(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	for i, r := range f.live {
		if r.ID == id {
			f.live = append(f.live[:i], f.live[i+1:]...)
			break
		}
	}
	for i, r := range f.dead {
		if r.ID == id {
			f.dead = append(f.dead[:i], f.dead[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeDocker) Usage(context.Context, []string, float64) docker.Usage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usage
}

func (f *fakeDocker) withLive(n int) *fakeDocker {
	for range n {
		f.next++
		f.live = append(f.live, docker.Replica{ID: fmt.Sprintf("id%d", f.next), Created: f.next})
	}
	return f
}

func (f *fakeDocker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}

type fakeQueue struct{ depth float64 }

func (q *fakeQueue) Depth(context.Context) (float64, error) { return q.depth, nil }

// worker is a service scaled on queue depth with a raw model, so decisions
// follow inputs exactly.
func worker() config.Service {
	return config.Service{
		Name: "worker", Min: 1, Max: 10,
		Template: config.Template{Image: "w"},
		Metrics:  []config.Metric{{Type: config.MetricQueueDepth, Target: 100}},
		Queue:    &config.Queue{Kind: config.QueueRedisList, URL: "redis://unused:6379", Name: "jobs"},
		Behavior: config.Behavior{Tolerance: 0.1, MaxStepUp: 4},
		Model:    config.Model{Kind: config.ModelRaw},
	}
}

type harness struct {
	c     *Controller
	dk    *fakeDocker
	q     *fakeQueue
	clock time.Time
}

func newHarness(t *testing.T, sc config.Service, dk *fakeDocker, dryRun bool, rec *replay.Recorder) *harness {
	t.Helper()
	cfg := &config.Config{Interval: 10 * time.Second, DryRun: dryRun, Services: []config.Service{sc}}
	c, err := New(cfg, dk, rec, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{c: c, dk: dk, q: &fakeQueue{}, clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c.now = func() time.Time { return h.clock }
	c.svcs[0].queue = h.q
	return h
}

func (h *harness) tick(d time.Duration) Status {
	h.clock = h.clock.Add(d)
	h.c.tickAll(context.Background())
	return h.c.Statuses()[0]
}

func TestScalesUpToMinOnStart(t *testing.T) {
	sc := worker()
	sc.Min = 2
	h := newHarness(t, sc, &fakeDocker{}, false, nil)
	st := h.tick(0)
	if h.dk.count() != 2 || st.Replicas != 2 || !strings.Contains(st.Reason, "below min") {
		t.Fatalf("replicas=%d status=%+v", h.dk.count(), st)
	}
}

func TestScalesUpOnQueueDepthWithStepLimit(t *testing.T) {
	h := newHarness(t, worker(), (&fakeDocker{}).withLive(1), false, nil)
	h.q.depth = 900 // needs 9 replicas at 100 per replica

	st := h.tick(0)
	if h.dk.count() != 5 {
		t.Fatalf("replicas = %d, want 5 (1 + max_step_up 4)", h.dk.count())
	}
	if st.Recommended != 9 || st.Desired != 5 || len(st.Signals) != 1 || st.Signals[0].Raw != 900 {
		t.Fatalf("status = %+v", st)
	}
	h.tick(10 * time.Second)
	if h.dk.count() != 9 {
		t.Fatalf("replicas = %d after second tick, want 9", h.dk.count())
	}
}

func TestScaleDownRemovesNewestFirst(t *testing.T) {
	h := newHarness(t, worker(), (&fakeDocker{}).withLive(4), false, nil)
	h.q.depth = 0
	h.tick(0)
	if h.dk.count() != 1 || h.dk.live[0].ID != "id1" {
		t.Fatalf("live = %+v, want only the oldest (id1)", h.dk.live)
	}
	removed := append([]string(nil), h.dk.removed...)
	slices.Sort(removed) // removals run in parallel
	if strings.Join(removed, ",") != "id2,id3,id4" {
		t.Fatalf("removed = %v", h.dk.removed)
	}
}

func TestDryRunDoesNotTouchContainers(t *testing.T) {
	h := newHarness(t, worker(), (&fakeDocker{}).withLive(1), true, nil)
	h.q.depth = 500
	st := h.tick(0)
	if h.dk.starts != 0 || h.dk.count() != 1 {
		t.Fatalf("dry run started containers: starts=%d", h.dk.starts)
	}
	if st.Desired != 5 || st.LastScale.IsZero() {
		t.Fatalf("status = %+v", st)
	}
}

func TestPartialStartFailureCommitsReachedCount(t *testing.T) {
	sc := worker()
	sc.Behavior.ScaleUpCooldown = time.Minute
	dk := (&fakeDocker{startErr: func(n int) error {
		if n > 2 {
			return errors.New("no capacity")
		}
		return nil
	}}).withLive(1)
	h := newHarness(t, sc, dk, false, nil)
	h.q.depth = 500

	st := h.tick(0)
	if dk.count() != 3 || st.Replicas != 3 || !strings.Contains(st.LastError, "no capacity") {
		t.Fatalf("replicas=%d status=%+v", dk.count(), st)
	}
	// The partial scale-up started the cooldown, so no retry storm.
	h.tick(10 * time.Second)
	if dk.starts != 4 {
		t.Fatalf("start attempts = %d, want 4 (no retry during cooldown)", dk.starts)
	}
	h.tick(time.Minute)
	if dk.starts <= 4 {
		t.Fatal("did not retry after cooldown")
	}
}

func TestStoppedReplicasAreCleanedUp(t *testing.T) {
	dk := (&fakeDocker{dead: []docker.Replica{{ID: "dead1"}}}).withLive(1)
	h := newHarness(t, worker(), dk, false, nil)
	h.q.depth = 100
	h.tick(0)
	if len(dk.removed) != 1 || dk.removed[0] != "dead1" || len(dk.dead) != 0 {
		t.Fatalf("removed = %v", dk.removed)
	}
}

func TestListErrorSurfacesInStatus(t *testing.T) {
	h := newHarness(t, worker(), &fakeDocker{listErr: errors.New("daemon down")}, false, nil)
	if st := h.tick(0); st.LastError != "daemon down" || h.dk.starts != 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestDockerStatsSignals(t *testing.T) {
	sc := worker()
	sc.Metrics = []config.Metric{{Type: config.MetricCPU, Target: 50}, {Type: config.MetricMemory, Target: 80}}
	sc.Queue = nil
	dk := (&fakeDocker{usage: docker.Usage{CPU: 100, CPUN: 2, Mem: 40, MemN: 2}}).withLive(2)
	h := newHarness(t, sc, dk, false, nil)
	st := h.tick(0)
	if dk.count() != 4 || len(st.Signals) != 2 {
		t.Fatalf("replicas=%d signals=%+v", dk.count(), st.Signals)
	}
}

func TestRecordsServiceTotals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rec.csv")
	rec, err := replay.NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, worker(), (&fakeDocker{}).withLive(2), true, rec)
	h.q.depth = 300
	h.tick(0)
	rec.Close()

	f, _ := os.Open(path)
	defer f.Close()
	rows, err := replay.ReadRows(f, "worker")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	if rows[0].Replicas != 2 || rows[0].Totals[config.MetricQueueDepth] != 300 {
		t.Fatalf("row = %+v, want total depth 300 at 2 replicas", rows[0])
	}
}

func TestHTTPEndpoints(t *testing.T) {
	h := newHarness(t, worker(), (&fakeDocker{}).withLive(1), false, nil)
	h.q.depth = 250
	h.tick(0)
	srv := httptest.NewServer(h.c.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var sts []Status
	if err := json.NewDecoder(resp.Body).Decode(&sts); err != nil || len(sts) != 1 || sts[0].Replicas != 3 {
		t.Fatalf("status = %+v, %v", sts, err)
	}
	resp.Body.Close()

	resp, err = srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`dockscale_replicas{service="worker"} 3`,
		`dockscale_desired_replicas{service="worker"} 3`,
		`dockscale_signal{service="worker",metric="queue_depth",kind="raw"} 250`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics missing %q:\n%s", want, body)
		}
	}
}

func TestMemoryJournalsGradesAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	sc := worker()
	sc.Behavior.ScaleUpCooldown = 5 * time.Minute
	open := func() (*memory.Store, *harness, *fakeDocker) {
		st, err := memory.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		dk := (&fakeDocker{}).withLive(1)
		cfg := &config.Config{Interval: 10 * time.Second, Services: []config.Service{sc},
			Memory: config.Memory{Dir: dir, SnapshotEvery: time.Minute, MaxStateAge: 15 * time.Minute, EvaluateAfter: time.Minute}}
		c, err := New(cfg, dk, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		h := &harness{c: c, dk: dk, q: &fakeQueue{}, clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
		c.now = func() time.Time { return h.clock }
		c.svcs[0].queue = h.q
		return st, h, dk
	}

	st, h, _ := open()
	h.q.depth = 300 // 1 replica, needs 3
	h.tick(0)
	h.q.depth = 250 // ~83 per replica afterwards: within target
	for range 7 {
		h.tick(10 * time.Second)
	}
	hist := h.c.History("worker", 10)
	if len(hist) != 1 || hist[0].Decision.From != 1 || hist[0].Decision.To != 3 {
		t.Fatalf("history = %+v", hist)
	}
	if hist[0].Outcome == nil || hist[0].Outcome.Verdict != memory.VerdictEffective {
		t.Fatalf("outcome = %+v, want effective", hist[0].Outcome)
	}
	h.c.Close()
	st.Close()

	// Restart: history comes back from the journal, and the restored scale-up
	// cooldown blocks an immediate scale-up the new process would otherwise make.
	st2, h2, dk2 := open()
	defer st2.Close()
	h2.clock = h.clock.Add(5 * time.Second)
	if got := h2.c.History("", 10); len(got) != 1 || got[0].Outcome == nil {
		t.Fatalf("history after restart = %+v", got)
	}
	h2.q.depth = 500
	h2.tick(0)
	if dk2.starts != 0 {
		t.Fatalf("restored cooldown ignored: %d starts", dk2.starts)
	}

	srv := httptest.NewServer(h2.c.Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/history?service=worker&limit=5")
	if err != nil {
		t.Fatal(err)
	}
	var entries []memory.Entry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil || len(entries) != 1 {
		t.Fatalf("/history = %+v, %v", entries, err)
	}
	resp.Body.Close()
}

func getJSON(t *testing.T, srv *httptest.Server, path string, v any) int {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v != nil && resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	return resp.StatusCode
}

func TestUIAndAPI(t *testing.T) {
	sc := worker()
	sc.Queue.URL = "redis://:hunter2@redis:6379/0" // must never be published
	sc.History = config.History{Cycle: 24 * time.Hour, Slot: 15 * time.Minute, Decay: 0.25, MinSamples: 2}
	h := newHarness(t, sc, (&fakeDocker{}).withLive(1), false, nil)
	h.q.depth = 250
	for range 100 { // 1000s: crosses a 15m profile slot boundary
		h.tick(10 * time.Second)
	}
	srv := httptest.NewServer(h.c.Handler())
	defer srv.Close()
	noRedirect := *srv.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := noRedirect.Get(srv.URL + "/")
	if err != nil || resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/" {
		t.Fatalf("GET / = %v %v, want redirect to /ui/", resp.StatusCode, err)
	}
	for _, path := range []string{"/ui/", "/ui/app.js", "/ui/style.css"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET %s = %v %v", path, resp.StatusCode, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(body) < 100 {
			t.Fatalf("%s looks empty (%d bytes)", path, len(body))
		}
	}

	resp, err = srv.Client().Get(srv.URL + "/api/overview")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "redis://") {
		t.Fatal("/api/overview leaked the queue URL")
	}
	var ov Overview
	if err := json.Unmarshal(raw, &ov); err != nil || len(ov.Services) != 1 {
		t.Fatalf("overview = %+v, %v", ov, err)
	}
	if !strings.Contains(string(raw), `"type":"queue_depth"`) || ov.Services[0].Status.Replicas != 3 {
		t.Fatalf("overview JSON shape unexpected: %s", raw)
	}

	var pts []Point
	if code := getJSON(t, srv, "/api/series?service=worker&window=5m", &pts); code != 200 || len(pts) != 31 {
		t.Fatalf("series 5m = %d points (status %d), want 31", len(pts), code)
	}
	if code := getJSON(t, srv, "/api/series?service=nope", nil); code != 404 {
		t.Fatalf("unknown service status = %d, want 404", code)
	}
	var rep map[string]any
	if code := getJSON(t, srv, "/api/report?service=worker", &rep); code != 200 || rep["ups"] == nil {
		t.Fatalf("report = %v (status %d)", rep, code)
	}
	var prof profileJSON
	if code := getJSON(t, srv, "/api/profile?service=worker", &prof); code != 200 || len(prof.Metrics["queue_depth"]) != 96 {
		t.Fatalf("profile = %+v (status %d), want 96 queue_depth slots", prof, code)
	}
}

func TestSeriesIsCapped(t *testing.T) {
	h := newHarness(t, worker(), (&fakeDocker{}).withLive(1), false, nil)
	h.q.depth = 100
	for range seriesLimit + 50 {
		h.tick(10 * time.Second)
	}
	pts, _ := h.c.Series("worker", time.Time{})
	if len(pts) != seriesLimit || !pts[len(pts)-1].At.Equal(h.clock) {
		t.Fatalf("series has %d points ending %v, want %d ending %v", len(pts), pts[len(pts)-1].At, seriesLimit, h.clock)
	}
}
