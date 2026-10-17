package controller

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dockscale/internal/memory"
	"dockscale/internal/ui"
)

// Handler serves:
//
//	/                 redirect to the web UI at /ui/
//	/healthz          liveness
//	/status, /history JSON (stable, for scripts)
//	/metrics          Prometheus text
//	/api/...          JSON for the web UI
//
// Every endpoint is read-only.
func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	mux.Handle("GET /ui/", http.StripPrefix("/ui/", ui.Handler()))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, c.Statuses(), true)
	})
	history := func(w http.ResponseWriter, r *http.Request) {
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit <= 0 {
			limit = 50
		}
		writeJSON(w, c.History(r.URL.Query().Get("service"), limit), r.URL.Path == "/history")
	}
	mux.HandleFunc("/history", history)
	mux.HandleFunc("GET /api/history", history)

	mux.HandleFunc("GET /api/overview", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, c.Overview(), false)
	})
	mux.HandleFunc("GET /api/series", func(w http.ResponseWriter, r *http.Request) {
		window, err := time.ParseDuration(r.URL.Query().Get("window"))
		if err != nil || window <= 0 {
			window = time.Hour
		}
		pts, ok := c.Series(r.URL.Query().Get("service"), c.now().Add(-window))
		if !ok {
			http.Error(w, "unknown service", http.StatusNotFound)
			return
		}
		writeJSON(w, pts, false)
	})
	mux.HandleFunc("GET /api/report", func(w http.ResponseWriter, r *http.Request) {
		rep, ok := c.Report(r.URL.Query().Get("service"))
		if !ok {
			http.Error(w, "unknown service", http.StatusNotFound)
			return
		}
		writeJSON(w, rep, false)
	})
	mux.HandleFunc("GET /api/profile", func(w http.ResponseWriter, r *http.Request) {
		p, ok := c.Profile(r.URL.Query().Get("service"))
		if !ok {
			http.Error(w, "no profile for this service", http.StatusNotFound)
			return
		}
		writeJSON(w, profileView(*p, c.now()), false)
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		var b strings.Builder
		b.WriteString("# TYPE dockscale_replicas gauge\n")
		b.WriteString("# TYPE dockscale_desired_replicas gauge\n")
		b.WriteString("# TYPE dockscale_signal gauge\n")
		for _, s := range c.Statuses() {
			fmt.Fprintf(&b, "dockscale_replicas{service=%q} %d\n", s.Service, s.Replicas)
			fmt.Fprintf(&b, "dockscale_desired_replicas{service=%q} %d\n", s.Service, s.Desired)
			for _, sig := range s.Signals {
				for kind, v := range map[string]float64{"raw": sig.Raw, "estimate": sig.Value, "target": sig.Target} {
					fmt.Fprintf(&b, "dockscale_signal{service=%q,metric=%q,kind=%q} %g\n", s.Service, sig.Metric, kind, v)
				}
			}
		}
		_, _ = w.Write([]byte(b.String()))
	})
	return mux
}

// writeJSON encodes v. The script-facing endpoints are indented for reading
// with curl; the UI's /api/* responses stay compact, since they're polled.
func writeJSON(w http.ResponseWriter, v any, indent bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	if indent {
		enc.SetIndent("", "  ")
	}
	_ = enc.Encode(v)
}

// profileJSON is the learned profile shaped for charting: per-slot mean and
// standard deviation for every metric, and which slot "now" falls in.
type profileJSON struct {
	CycleSeconds float64                `json:"cycle_seconds"`
	SlotSeconds  float64                `json:"slot_seconds"`
	NowSlot      int                    `json:"now_slot"`
	Metrics      map[string][]slotPoint `json:"metrics"`
}

type slotPoint struct {
	N      int     `json:"n"`
	Mean   float64 `json:"mean"`
	StdDev float64 `json:"stddev"`
}

func profileView(p memory.ProfileState, now time.Time) profileJSON {
	out := profileJSON{
		CycleSeconds: p.Cycle.Seconds(), SlotSeconds: p.Slot.Seconds(),
		Metrics: map[string][]slotPoint{},
	}
	if p.Slot > 0 && p.Cycle > 0 {
		n, cyc := now.UnixNano(), int64(p.Cycle)
		out.NowSlot = int((((n % cyc) + cyc) % cyc) / int64(p.Slot))
	}
	for metric, slots := range p.Slots {
		pts := make([]slotPoint, len(slots))
		for i, s := range slots {
			pts[i] = slotPoint{N: s.N, Mean: s.Mean, StdDev: math.Sqrt(s.Var)}
		}
		out.Metrics[metric] = pts
	}
	return out
}
