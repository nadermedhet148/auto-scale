# CLAUDE.md

Guidance for AI coding sessions (and humans) working in this repo. Read this
first, then `docs/decisions/README.md` for why things are the way they are.

## What this is

dockscale: a single-host horizontal autoscaler for Docker containers, written
in Go. It uses statistical models only, with no ML (see ADR 0003). Module path:
`dockscale`.

## Commands

```sh
go test ./...                                         # all tests (Go 1.26+ required)
go vet ./...
gofmt -l .                                            # must print nothing
go test -run '^$' -bench . ./internal/...             # benchmarks
go test -run '^$' -fuzz FuzzParse ./internal/metrics  # fuzzing
go run ./cmd/dockscale-replay generate -shape step -days 4 -out load.csv
go run ./cmd/dockscale-replay -in load.csv -config examples/policies/holt.yaml -config examples/policies/holt_prewarm.yaml
go run ./cmd/dockscale-memory report -dir ./mem -config examples/dockscale.yaml
docker compose up --build -d                          # demo: Traefik + dockscale + whoami replicas
go run ./cmd/dockscale-demo                           # web UI with fake Docker + synthetic load: http://localhost:9090
node --check internal/ui/static/app.js                # syntax-check the UI script
```

`go test -race` needs cgo and a C compiler. It isn't available on every dev
machine.

## Layout

| Package | Role | Pure? |
|---|---|---|
| `cmd/dockscale` | Main binary: wiring, HTTP, signals | no |
| `cmd/dockscale-replay` | Offline simulator and load generator | no |
| `cmd/dockscale-memory` | Inspect the journal, report, profile | no |
| `cmd/dockscale-demo` | Real controller + UI against a fake Docker; for UI work | no |
| `internal/config` | YAML, defaults, validation | yes |
| `internal/engine` | Per-service decision core: models + profile + policy, snapshot/restore | **yes** |
| `internal/model` | Outlier filter, EWMA, Holt, Holt-Winters | **yes** |
| `internal/policy` | Target tracking, cooldowns, stabilization | **yes** |
| `internal/memory` | Profile, outcome evaluator, report (pure); `store.go` (files) | **yes**, except `store.go` |
| `internal/controller` | The live control loop and HTTP handlers | no |
| `internal/docker` | Docker Engine API wrapper (`moby/moby/client`) | no |
| `internal/metrics` | Prometheus parser, histogram quantile, Traefik scraper | parser yes |
| `internal/queue` | Redis list and RabbitMQ backlog | no |
| `internal/replay` | Recorder, simulator, synthetic load | yes, except the recorder |
| `internal/ui` | Embedded dashboard (`static/index.html`, `style.css`, `app.js`) | n/a |

## Rules

1. **Keep the pure packages pure.** No I/O, goroutines or `time.Now()` in
   `engine`, `model`, `policy` or `memory` (outside `store.go`). Time is passed
   in. The live controller and replay share the engine, so they must
   produce identical decisions (ADR 0006).
2. **Docker is the source of truth for replicas.** Never cache the count across
   ticks. Never touch a container without the `dockscale.managed=true` label
   (ADR 0002).
3. **Minimal dependencies.** Don't add a module for something small enough to
   write and test in-repo (ADR 0007). Ask before adding any dependency.
4. **Every decision is explainable.** New logic that affects the replica count
   must show up in the `Reason` string or as a `policy.Signal`.
5. **Seasonal or slotted state is indexed by wall-clock time, never by tick
   count** (ADR 0011).
6. **New learned state must be persisted.** Add it to `engine.Snapshot`, decide
   whether it's short-term (fresh-only) or long-term, and check it against the
   config on restore (ADR 0010).
7. **Comment density:** package docs and exported-type docs, plus comments on
   non-obvious *why*. No comments narrating what the code says.
8. **The UI is read-only and dependency-free** (ADR 0014). No endpoints that
   change state, no npm or CDN. Insert API data with `textContent` (the `h()`
   helper), never `innerHTML`. Never publish image, env, labels or URLs in
   `/api/*`; `TestUIAndAPI` guards this. After UI changes, run the demo and
   look at it, including light mode, dark mode and phone width.
9. **Tests:**
   - The controller is tested through the `Docker` interface with
     `fakeDocker` and an injected clock (`c.now`).
   - The Docker wrapper is tested against `fakeEngine`, an in-memory Engine
     API.
   - Use fixed timestamps, never `time.Now()`, in tests of pure packages.
   - When a test claims to catch a bug, check that it does: break the code
     temporarily and confirm the test fails.

## Recipes

**Add a metric type:**
1. Add a constant to `config` and to `AllMetrics`, and validate its
   requirements.
2. Collect it as a per-replica value in `controller.collect`.
3. Mark it in `engine.Additive` if its total splits across replicas.
4. Test it, and document it in `docs/metrics.md` and `docs/operations.md`.

**Add a model:**
1. Implement `model.Estimator` (`Update(t, x)`).
2. Add it to `NewPipeline`, `State` and `Restore`, and to the kinds in
   `config.validate`.
3. Add benchmarks, then compare it with `dockscale-replay` before recommending
   it.

**Change a decision recorded in an ADR:** write a new ADR that supersedes the
old one. Don't rewrite accepted ADRs. Note it in `CHANGELOG.md`.

## Before finishing a change

- [ ] `gofmt -l .` is empty, and `go vet ./...` and `go test ./...` pass
- [ ] Docs in `docs/` match the behaviour, and the Mermaid diagrams still parse
- [ ] `CHANGELOG.md` has an "Unreleased" entry
- [ ] A new ADR exists if a design decision was made
- [ ] `examples/*.yaml` still load (`TestExamplesLoad`)
