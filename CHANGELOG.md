# Changelog

Notable changes to dockscale. The reasoning behind design choices is in
[docs/decisions/](docs/decisions/README.md).

## Unreleased

### Added
- **Runtime memory** (`memory.dir`), see [docs/memory.md](docs/memory.md):
  - Learned state is snapshotted to `state-<service>.json` (models, cooldowns,
    stabilization window, load profile) and restored at startup. Short-term
    state is used only from fresh snapshots. ([0010](docs/decisions/0010-runtime-memory-as-files.md))
  - Decision journal `journal.jsonl`: every scaling action with its reason and
    signals, graded after `evaluate_after` as effective, insufficient or
    unnecessary (scale-ups), or ok or premature (scale-downs).
    ([0012](docs/decisions/0012-grade-decisions-by-outcome.md))
  - A load profile per time slot (`history`), and `history.prewarm` to scale
    ahead of recurring peaks. ([0013](docs/decisions/0013-prewarm-as-a-policy-signal.md))
  - `GET /history`, the recent decisions with outcomes.
  - `dockscale-memory` CLI: `decisions`, `report` (tuning advice), `profile`.
  - `dockscale-replay -memory-dir` writes the same journal and state offline,
    and the replay summary shows verdict counts.
  - `dockscale-replay generate -shape step` for business-hours load.
- Architecture decision records in `docs/decisions/`, this changelog, and
  `CLAUDE.md`.

### Changed
- Holt-Winters seasons are aligned to the wall clock, so they survive gaps and
  restarts. `model.Estimator.Update` now takes the sample time.
  ([0011](docs/decisions/0011-wall-clock-aligned-seasonality.md))
- `engine.New` takes the control-loop interval; `controller.New` takes an
  optional `*memory.Store`.
- The demo compose file mounts a `dockscale-memory` volume.

## 0.1.0 (2026-10-17)

First commit (`fc973b3`).

### Added
- Single-host autoscaler for Docker containers, with Traefik routing replicas
  discovered by label. ([0002](docs/decisions/0002-single-host-docker-api-labels.md), [0005](docs/decisions/0005-traefik-routing-and-metrics.md))
- Signals: CPU and memory (Docker stats), rps and p95 latency (Traefik),
  queue depth (Redis list, RabbitMQ).
- Models: MAD outlier filter, then EWMA, Holt or Holt-Winters. ([0003](docs/decisions/0003-statistical-models-no-ml.md))
- Policy: target tracking, tolerance, cooldowns, scale-down stabilization,
  step limits. ([0004](docs/decisions/0004-target-tracking-asymmetric-policy.md))
- `dockscale-replay`: record live load, generate synthetic load, and compare
  configs offline on the shared pure engine. ([0006](docs/decisions/0006-pure-engine-shared-with-replay.md), [0009](docs/decisions/0009-recordings-store-totals.md))
- `/status`, `/metrics`, `/healthz`, and `-dry-run`.
- Tests (including against a fake Docker Engine API), benchmarks, fuzz tests.
- Documentation in `docs/`.

### Fixed during development
- Moved from `docker/docker` v27 to `moby/moby/client` with Go 1.26 after the
  old SDK stopped building. ([0008](docs/decisions/0008-moby-client-and-go-1-26.md))
- Histogram quantile could overshoot its bucket bound by floating-point
  rounding (found by the fuzz test).
- The demo now sets finer Traefik latency buckets; the defaults are too coarse
  for p95.
