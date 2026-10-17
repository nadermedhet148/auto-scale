// Command dockscale-replay runs recorded (or synthetic) load through the
// dockscale decision engine offline, to compare policies before deploying.
//
//	dockscale-replay generate -out load.csv -days 2
//	dockscale-replay -in load.csv -config a.yaml -config b.yaml -timeline out.csv
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"dockscale/internal/config"
	"dockscale/internal/memory"
	"dockscale/internal/replay"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	var err error
	if len(os.Args) > 1 && os.Args[1] == "generate" {
		err = generate(os.Args[2:])
	} else {
		err = simulate(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func generate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	out := fs.String("out", "load.csv", "output CSV")
	service := fs.String("service", "web", "service name to write")
	days := fs.Float64("days", 2, "length of the recording in days")
	interval := fs.Duration("interval", 10*time.Second, "sample spacing; match the config's interval")
	base := fs.Float64("base", 20, "overnight total rps")
	peak := fs.Float64("peak", 400, "midday total rps")
	noise := fs.Float64("noise", 0.08, "noise std-dev as a fraction of the level")
	spike := fs.Float64("spike-p", 0.002, "per-tick probability of a 2.5x spike lasting 3 ticks")
	cpuPerRPS := fs.Float64("cpu-per-rps", 0.5, "CPU % consumed per request/sec")
	seed := fs.Uint64("seed", 1, "random seed")
	shape := fs.String("shape", "sine", "daily pattern: sine (smooth ramp) or step (09:00-17:00 business hours)")
	_ = fs.Parse(args)

	rows := replay.Synthesize(*service, replay.SynthOptions{
		Start:    time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		Duration: time.Duration(*days * float64(24*time.Hour)),
		Shape:    *shape, Interval: *interval, Base: *base, Peak: *peak, Noise: *noise, SpikeP: *spike, Seed: *seed,
	}, *cpuPerRPS)
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := replay.WriteRows(f, rows); err != nil {
		return err
	}
	fmt.Printf("wrote %d rows to %s\n", len(rows), *out)
	return nil
}

func simulate(args []string) error {
	fs := flag.NewFlagSet("dockscale-replay", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "dockscale config to evaluate (repeat to compare)")
	in := fs.String("in", "load.csv", "recording made with `dockscale -record` or `generate`")
	service := fs.String("service", "", "service to replay (default: first in each config)")
	start := fs.Int("start", 0, "initial replicas (default: min)")
	startup := fs.Duration("startup-delay", 10*time.Second, "time before a new replica takes load")
	timeline := fs.String("timeline", "", "write a per-tick CSV for the first config")
	memDir := fs.String("memory-dir", "", "write the first config's decision journal and learned state here, for dockscale-memory")
	_ = fs.Parse(args)
	if len(configs) == 0 {
		configs = multiFlag{"examples/dockscale.yaml"}
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "config\tmodel\tprewarm\tscale-ups\tscale-downs\tpeak\treplica-h\toverload\toverload %\tups eff/insuf/unnec\tdowns ok/prem\t")
	var recordedHours float64
	for i, path := range configs {
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		sc, err := pickService(cfg, *service)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		f, err := os.Open(*in)
		if err != nil {
			return err
		}
		rows, err := replay.ReadRows(f, sc.Name)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", *in, err)
		}
		if len(rows) == 0 {
			return fmt.Errorf("%s has no rows for service %q", *in, sc.Name)
		}

		opt := replay.Options{Start: *start, StartupDelay: *startup, EvaluateAfter: cfg.Memory.EvaluateAfter}
		var store *memory.Store
		if i == 0 && *memDir != "" {
			if store, err = memory.Open(*memDir); err != nil {
				return err
			}
			opt.Journal = func(r memory.Record) { _ = store.Append(r) }
		}
		ticks, sum := replay.Simulate(sc, cfg.Interval, rows, opt)
		if store != nil {
			if err := store.SaveState(sc.Name, sum.Final); err != nil {
				return err
			}
			store.Close()
		}
		if sum.IntervalWarning {
			fmt.Fprintf(os.Stderr, "warning: %s: recording spacing %v differs from interval %v; cooldowns and forecasts will be off\n",
				path, sum.MedianStep, cfg.Interval)
		}
		pct := 0.0
		if sum.Duration > 0 {
			pct = 100 * float64(sum.OverloadTime) / float64(sum.Duration)
		}
		v := sum.Verdicts
		fmt.Fprintf(tw, "%s\t%s\t%v\t%d\t%d\t%d\t%.1f\t%s\t%.2f\t%d/%d/%d\t%d/%d\t\n", path, sc.Model.Kind, sc.History.Prewarm,
			sum.ScaleUps, sum.ScaleDowns, sum.PeakReplicas, sum.ReplicaHours, sum.OverloadTime.Round(time.Second), pct,
			v[memory.VerdictEffective], v[memory.VerdictInsufficient], v[memory.VerdictUnnecessary], v[memory.VerdictOK], v[memory.VerdictPremature])
		recordedHours = sum.RecordedHours

		if i == 0 && *timeline != "" {
			if err := writeTimeline(*timeline, sc, ticks); err != nil {
				return err
			}
		}
	}
	tw.Flush()
	if recordedHours > 0 {
		fmt.Printf("\nrecording itself ran %.1f replica-hours\n", recordedHours)
	}
	return nil
}

func pickService(cfg *config.Config, name string) (config.Service, error) {
	if name == "" {
		return cfg.Services[0], nil
	}
	for _, s := range cfg.Services {
		if s.Name == name {
			return s, nil
		}
	}
	return config.Service{}, fmt.Errorf("no service %q", name)
}

func writeTimeline(path string, sc config.Service, ticks []replay.Tick) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := []string{"timestamp", "replicas", "ready", "desired", "overload"}
	for _, m := range sc.Metrics {
		head = append(head, m.Type+"_raw", m.Type+"_estimate", m.Type+"_target")
	}
	head = append(head, "reason")
	w := csv.NewWriter(f)
	_ = w.Write(head)
	for _, t := range ticks {
		rec := []string{t.At.Format(time.RFC3339), strconv.Itoa(t.Replicas), strconv.Itoa(t.Ready),
			strconv.Itoa(t.Desired), strconv.FormatBool(t.Overload)}
		byMetric := map[string][3]string{}
		for _, s := range t.Signals {
			byMetric[s.Metric] = [3]string{ff(s.Raw), ff(s.Value), ff(s.Target)}
		}
		for _, m := range sc.Metrics {
			v := byMetric[m.Type]
			rec = append(rec, v[0], v[1], v[2])
		}
		_ = w.Write(append(rec, t.Reason))
	}
	w.Flush()
	return w.Error()
}

func ff(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
