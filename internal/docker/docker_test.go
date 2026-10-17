package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"dockscale/internal/config"
)

// fakeEngine is an in-memory stand-in for the Docker Engine API, covering
// the endpoints dockscale calls.
type fakeEngine struct {
	mu         sync.Mutex
	images     map[string]bool
	containers map[string]*fakeContainer
	order      []string
	pulls      []string
	stops      map[string]string // id -> stop timeout query
	nextID     int
}

type fakeContainer struct {
	ID      string
	Name    string
	Created int64
	State   string
	Labels  map[string]string
	Create  map[string]any // raw create request body
	stats   []string       // successive stats responses; the last repeats
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{images: map[string]bool{}, containers: map[string]*fakeContainer{}, stops: map[string]string{}}
}

func (f *fakeEngine) add(c *fakeContainer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[c.ID] = c
	f.order = append(f.order, c.ID)
}

var versionPrefix = regexp.MustCompile(`^/v1\.\d+`)

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := versionPrefix.ReplaceAllString(r.URL.Path, "")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	notFound := func(what string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"message":"No such %s"}`, what)
	}

	switch {
	case path == "/_ping":
		w.Header().Set("Api-Version", "1.47")
		fmt.Fprint(w, "OK")

	case r.Method == http.MethodGet && path == "/containers/json":
		var filters map[string]map[string]bool
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		out := []map[string]any{}
		for _, id := range f.order {
			c := f.containers[id]
			if c == nil || !matchLabels(c.Labels, filters["label"]) {
				continue
			}
			out = append(out, map[string]any{
				"Id": c.ID, "Names": []string{"/" + c.Name}, "Created": c.Created, "State": c.State, "Labels": c.Labels,
			})
		}
		_ = json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodPost && path == "/containers/create":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !f.images[body["Image"].(string)] {
			notFound("image")
			return
		}
		f.nextID++
		id := fmt.Sprintf("%064d", f.nextID)
		labels := map[string]string{}
		for k, v := range body["Labels"].(map[string]any) {
			labels[k] = v.(string)
		}
		f.containers[id] = &fakeContainer{ID: id, Name: r.URL.Query().Get("name"), Created: int64(f.nextID),
			State: "created", Labels: labels, Create: body}
		f.order = append(f.order, id)
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Warnings": []string{}})

	case r.Method == http.MethodPost && path == "/images/create":
		ref := r.URL.Query().Get("fromImage")
		if tag := r.URL.Query().Get("tag"); tag != "" {
			ref += ":" + tag
		}
		f.pulls = append(f.pulls, ref)
		f.images[strings.TrimPrefix(strings.TrimPrefix(ref, "docker.io/library/"), "docker.io/")] = true
		fmt.Fprintln(w, `{"status":"Downloaded"}`)

	case len(parts) == 3 && parts[0] == "containers" && r.Method == http.MethodPost:
		c := f.containers[parts[1]]
		if c == nil {
			notFound("container")
			return
		}
		switch parts[2] {
		case "start":
			c.State = "running"
		case "stop":
			c.State = "exited"
			f.stops[c.ID] = r.URL.Query().Get("t")
		}
		w.WriteHeader(http.StatusNoContent)

	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "stats":
		c := f.containers[parts[1]]
		if c == nil || len(c.stats) == 0 {
			notFound("container")
			return
		}
		fmt.Fprint(w, c.stats[0])
		if len(c.stats) > 1 {
			c.stats = c.stats[1:]
		}

	case len(parts) == 2 && parts[0] == "containers" && r.Method == http.MethodDelete:
		if f.containers[parts[1]] == nil {
			notFound("container")
			return
		}
		delete(f.containers, parts[1])
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, `{"message":"unexpected `+r.Method+" "+path+`"}`, http.StatusNotImplemented)
	}
}

func matchLabels(have map[string]string, want map[string]bool) bool {
	for kv := range want {
		k, v, _ := strings.Cut(kv, "=")
		if have[k] != v {
			return false
		}
	}
	return true
}

func newTestClient(t testing.TB, f *fakeEngine) *Client {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(client.WithHost("tcp://" + strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func managed(service string) map[string]string {
	return map[string]string{LabelManaged: "true", LabelService: service}
}

func statsBody(total, system uint64, online int, memUsage, inactive, memLimit uint64) string {
	return fmt.Sprintf(`{"cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d,"online_cpus":%d},`+
		`"memory_stats":{"usage":%d,"limit":%d,"stats":{"inactive_file":%d}}}`, total, system, online, memUsage, memLimit, inactive)
}

func TestReplicasSplitsLiveAndDeadOldestFirst(t *testing.T) {
	f := newFakeEngine()
	f.add(&fakeContainer{ID: "c3", Name: "web-c", Created: 30, State: "running", Labels: managed("web")})
	f.add(&fakeContainer{ID: "c1", Name: "web-a", Created: 10, State: "running", Labels: managed("web")})
	f.add(&fakeContainer{ID: "c2", Name: "web-b", Created: 20, State: "exited", Labels: managed("web")})
	f.add(&fakeContainer{ID: "o1", Name: "api-a", Created: 5, State: "running", Labels: managed("api")})
	f.add(&fakeContainer{ID: "u1", Name: "unmanaged", Created: 1, State: "running", Labels: map[string]string{LabelService: "web"}})

	live, dead, err := newTestClient(t, f).Replicas(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 || live[0].ID != "c1" || live[1].ID != "c3" || live[0].Name != "web-a" {
		t.Fatalf("live = %+v", live)
	}
	if len(dead) != 1 || dead[0].ID != "c2" {
		t.Fatalf("dead = %+v", dead)
	}
}

func TestStartPullsMissingImageAndAppliesTemplate(t *testing.T) {
	f := newFakeEngine()
	c := newTestClient(t, f)
	svc := config.Service{Name: "web", Template: config.Template{
		Image: "traefik/whoami:v1", Env: []string{"A=1"}, Network: "netx", CPUs: 0.5, MemoryBytes: 128 << 20,
		Labels: map[string]string{"traefik.enable": "true"},
	}}

	id, err := c.Start(context.Background(), svc)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.pulls) != 1 || f.pulls[0] != "docker.io/traefik/whoami:v1" {
		t.Fatalf("pulls = %v", f.pulls)
	}
	ct := f.containers[id]
	if ct.State != "running" || !strings.HasPrefix(ct.Name, "web-") {
		t.Fatalf("container = %+v", ct)
	}
	for k, v := range map[string]string{LabelManaged: "true", LabelService: "web", "traefik.enable": "true"} {
		if ct.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, ct.Labels[k], v)
		}
	}
	hc := ct.Create["HostConfig"].(map[string]any)
	if hc["NanoCpus"].(float64) != 5e8 || hc["Memory"].(float64) != 128<<20 || hc["NetworkMode"] != "netx" {
		t.Errorf("host config = %v", hc)
	}
	if rp := hc["RestartPolicy"].(map[string]any); rp["Name"] != "unless-stopped" {
		t.Errorf("restart policy = %v", rp)
	}
	eps := ct.Create["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	if _, ok := eps["netx"]; !ok {
		t.Errorf("endpoints = %v", eps)
	}
}

func TestStartDoesNotPullPresentImage(t *testing.T) {
	f := newFakeEngine()
	f.images["img"] = true
	if _, err := newTestClient(t, f).Start(context.Background(), config.Service{Name: "w", Template: config.Template{Image: "img"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.pulls) != 0 {
		t.Fatalf("unexpected pulls %v", f.pulls)
	}
}

func TestRemoveStopsWithGraceAndDeletes(t *testing.T) {
	f := newFakeEngine()
	f.add(&fakeContainer{ID: "c1", State: "running", Labels: managed("web")})
	c := newTestClient(t, f)

	if err := c.Remove(context.Background(), "c1", 7*time.Second); err != nil {
		t.Fatal(err)
	}
	if f.stops["c1"] != "7" {
		t.Fatalf("stop timeout = %q, want 7", f.stops["c1"])
	}
	if _, ok := f.containers["c1"]; ok {
		t.Fatal("container not deleted")
	}
	if err := c.Remove(context.Background(), "gone", time.Second); err != nil {
		t.Fatalf("removing a missing container should succeed: %v", err)
	}
}

func TestUsageComputesCPUFromConsecutiveSamples(t *testing.T) {
	f := newFakeEngine()
	f.add(&fakeContainer{ID: "c1", State: "running", stats: []string{
		statsBody(1e9, 100e9, 4, 100<<20, 20<<20, 200<<20),
		// +0.5s container CPU over +2s system CPU on 4 cores = 1 core.
		statsBody(1.5e9, 102e9, 4, 100<<20, 20<<20, 200<<20),
	}})
	f.add(&fakeContainer{ID: "c2", State: "running", stats: []string{
		statsBody(0, 100e9, 4, 50<<20, 0, 200<<20),
		statsBody(0, 102e9, 4, 50<<20, 0, 200<<20),
	}})
	c := newTestClient(t, f)
	ids := []string{"c1", "c2"}

	u := c.Usage(context.Background(), ids, 0.5)
	if u.CPUN != 0 || u.MemN != 2 {
		t.Fatalf("first sample: %+v (CPU needs two samples)", u)
	}
	// Memory excludes inactive file cache: (80 + 50) / 2 MiB of 200 MiB.
	if want := 32.5; u.Mem != want {
		t.Fatalf("mem = %v, want %v", u.Mem, want)
	}

	u = c.Usage(context.Background(), ids, 0.5)
	// c1: 1 core of a 0.5-core limit = 200%; c2: 0%.
	if u.CPUN != 2 || u.CPU != 100 {
		t.Fatalf("second sample: %+v, want avg CPU 100", u)
	}
}

func TestUsageCountsErrors(t *testing.T) {
	f := newFakeEngine()
	u := newTestClient(t, f).Usage(context.Background(), []string{"missing"}, 0)
	if u.Errors != 1 || u.CPUN != 0 || u.MemN != 0 {
		t.Fatalf("usage = %+v", u)
	}
}
