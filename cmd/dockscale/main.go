// Command dockscale autoscales Docker containers on a single host.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dockscale/internal/config"
	"dockscale/internal/controller"
	"dockscale/internal/docker"
	"dockscale/internal/memory"
	"dockscale/internal/replay"
)

func main() {
	cfgPath := flag.String("config", "dockscale.yaml", "path to the config file")
	dryRun := flag.Bool("dry-run", false, "log scaling decisions without changing containers")
	verbose := flag.Bool("v", false, "debug logging (logs every hold decision)")
	record := flag.String("record", "", "append each tick's load to this CSV, for dockscale-replay")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(*cfgPath, *dryRun, *record, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, dryRun bool, recordPath string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if dryRun {
		cfg.DryRun = true
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dk, err := docker.New()
	if err != nil {
		return err
	}
	if err := dk.Ping(ctx); err != nil {
		return err
	}

	var rec *replay.Recorder
	if recordPath != "" {
		if rec, err = replay.NewRecorder(recordPath); err != nil {
			return err
		}
		defer rec.Close()
	}

	var mem *memory.Store
	if cfg.Memory.Dir != "" {
		if mem, err = memory.Open(cfg.Memory.Dir); err != nil {
			return err
		}
		defer mem.Close()
	}

	ctl, err := controller.New(cfg, dk, rec, mem, log)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: ctl.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
		}
	}()

	log.Info("dockscale started", "services", len(cfg.Services), "interval", cfg.Interval, "listen", cfg.Listen, "dry_run", cfg.DryRun, "memory", cfg.Memory.Dir)
	ctl.Run(ctx)
	ctl.Close() // save learned state before exiting

	// Replicas are left running on shutdown; the next start adopts them by label.
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
