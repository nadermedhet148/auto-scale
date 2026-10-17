// Command dockscale-memory inspects what dockscale has remembered: past
// decisions and their outcomes, a tuning report, and learned load profiles.
//
//	dockscale-memory decisions -dir /var/lib/dockscale -since 24h
//	dockscale-memory report    -dir /var/lib/dockscale -config dockscale.yaml
//	dockscale-memory profile   -dir /var/lib/dockscale -service web -metric rps
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"dockscale/internal/config"
	"dockscale/internal/engine"
	"dockscale/internal/memory"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "decisions":
		err = decisions(os.Args[2:])
	case "report":
		err = report(os.Args[2:])
	case "profile":
		err = profile(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: dockscale-memory decisions|report|profile [flags]")
	os.Exit(2)
}

func decisions(args []string) error {
	fs := flag.NewFlagSet("decisions", flag.ExitOnError)
	dir := fs.String("dir", "/var/lib/dockscale", "memory directory (memory.dir)")
	service := fs.String("service", "", "only this service")
	since := fs.Duration("since", 24*time.Hour, "how far back to look, from the newest journal entry")
	limit := fs.Int("limit", 50, "most recent N decisions")
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)

	recs, err := memory.ReadJournal(*dir)
	if err != nil {
		return err
	}
	entries := memory.Join(recs, *service, latest(recs).Add(-*since))
	if len(entries) > *limit {
		entries = entries[len(entries)-*limit:]
	}
	if *asJSON {
		return printJSON(entries)
	}
	if len(entries) == 0 {
		fmt.Println("no decisions in this period")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME (UTC)\tSERVICE\tCHANGE\tVERDICT\tOVERLOAD\tREASON")
	for _, e := range entries {
		d := e.Decision
		change := fmt.Sprintf("%d→%d", d.From, d.To)
		if d.To != d.Wanted {
			change += fmt.Sprintf(" (wanted %d)", d.Wanted)
		}
		verdict, overload := "pending", ""
		if d.DryRun {
			verdict = "dry-run"
		}
		if e.Outcome != nil {
			verdict = e.Outcome.Verdict
			overload = fmt.Sprintf("%.0f%%", 100*e.Outcome.OverloadFraction)
		}
		if d.Error != "" {
			verdict += " (error)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", d.At.UTC().Format("01-02 15:04:05"), d.Service, change, verdict, overload, d.Reason)
	}
	return tw.Flush()
}

func report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	dir := fs.String("dir", "/var/lib/dockscale", "memory directory (memory.dir)")
	cfgPath := fs.String("config", "dockscale.yaml", "current config, so advice can quote its settings")
	service := fs.String("service", "", "only this service (default: all)")
	since := fs.Duration("since", 7*24*time.Hour, "how far back to look, from the newest journal entry")
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	recs, err := memory.ReadJournal(*dir)
	if err != nil {
		return err
	}
	now := latest(recs)
	from := now.Add(-*since)
	var reports []memory.Report
	for _, sc := range cfg.Services {
		if *service != "" && sc.Name != *service {
			continue
		}
		reports = append(reports, memory.Analyze(memory.Join(recs, sc.Name, from), sc, from, now))
	}
	if *asJSON {
		return printJSON(reports)
	}
	for _, r := range reports {
		fmt.Printf("== %s (last %v)\n", r.Service, since.Round(time.Hour))
		fmt.Printf("   %d decisions (%d up, %d down), %.2f per hour, %d failed\n", r.Decisions, r.Ups, r.Downs, r.PerHour, r.Failed)
		fmt.Printf("   scale-ups:   %d effective, %d insufficient, %d unnecessary\n",
			r.Verdicts[memory.VerdictEffective], r.Verdicts[memory.VerdictInsufficient], r.Verdicts[memory.VerdictUnnecessary])
		fmt.Printf("   scale-downs: %d ok, %d premature\n", r.Verdicts[memory.VerdictOK], r.Verdicts[memory.VerdictPremature])
		for _, a := range r.Advice {
			fmt.Println("   → " + a)
		}
		fmt.Println()
	}
	return nil
}

func profile(args []string) error {
	fs := flag.NewFlagSet("profile", flag.ExitOnError)
	dir := fs.String("dir", "/var/lib/dockscale", "memory directory (memory.dir)")
	service := fs.String("service", "", "service name (required)")
	metric := fs.String("metric", config.MetricRPS, "metric to show")
	_ = fs.Parse(args)
	if *service == "" {
		return errors.New("-service is required")
	}

	st, err := memory.Open(*dir)
	if err != nil {
		return err
	}
	defer st.Close()
	var snap engine.Snapshot
	found, err := st.LoadState(*service, &snap)
	if err != nil {
		return err
	}
	if !found || snap.Profile == nil {
		return fmt.Errorf("no learned profile for %q in %s", *service, *dir)
	}
	p := memory.NewProfile(snap.Profile.Cycle, snap.Profile.Slot, 1)
	if err := p.Restore(*snap.Profile); err != nil {
		return err
	}
	slots := p.Slots(*metric)
	if slots == nil {
		return fmt.Errorf("no profile for metric %q (learned: %s)", *metric, strings.Join(p.Metrics(), ", "))
	}

	peak := 0.0
	for _, s := range slots {
		peak = math.Max(peak, s.Mean)
	}
	fmt.Printf("%s %s: cycle %v, slot %v, saved %s\n\n", *service, *metric, snap.Profile.Cycle, snap.Profile.Slot, snap.SavedAt.UTC().Format(time.RFC3339))
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SLOT (UTC)\tVISITS\tMEAN\tSTDDEV\t")
	weekly := snap.Profile.Cycle >= 7*24*time.Hour
	for i, s := range slots {
		if s.N == 0 {
			continue
		}
		at := time.Unix(0, 0).UTC().Add(time.Duration(i) * snap.Profile.Slot)
		label := at.Format("15:04")
		if weekly {
			label = at.Format("Mon 15:04")
		}
		bar := ""
		if peak > 0 {
			bar = strings.Repeat("█", int(math.Round(20*s.Mean/peak)))
		}
		fmt.Fprintf(tw, "%s\t%d\t%.1f\t%.1f\t%s\n", label, s.N, s.Mean, math.Sqrt(s.Var), bar)
	}
	return tw.Flush()
}

// latest is the time of the newest journal record, so -since windows work
// the same for a live journal and for one written by dockscale-replay.
func latest(recs []memory.Record) time.Time {
	var t time.Time
	for _, r := range recs {
		switch {
		case r.Decision != nil && r.Decision.At.After(t):
			t = r.Decision.At
		case r.Outcome != nil && r.Outcome.At.After(t):
			t = r.Outcome.At
		}
	}
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
