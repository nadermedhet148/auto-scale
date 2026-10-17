package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Handler serves /healthz, /status and /history (JSON), and /metrics
// (Prometheus text).
func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(c.Statuses())
	})
	mux.HandleFunc("/history", func(w http.ResponseWriter, r *http.Request) {
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit <= 0 {
			limit = 50
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(c.History(r.URL.Query().Get("service"), limit))
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
