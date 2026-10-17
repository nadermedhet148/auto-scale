# Architecture decision records

Each file records one decision: the context, what was decided, the
alternatives considered, and the consequences. Together they're the project's
memory of *why* it is the way it is.

| # | Decision | Status |
|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | Accepted |
| [0002](0002-single-host-docker-api-labels.md) | Single host, Docker Engine API, labels as the source of truth | Accepted |
| [0003](0003-statistical-models-no-ml.md) | Statistical models, no ML | Accepted |
| [0004](0004-target-tracking-asymmetric-policy.md) | Target tracking with fast scale-up and slow scale-down | Accepted |
| [0005](0005-traefik-routing-and-metrics.md) | Traefik for routing and request metrics | Accepted |
| [0006](0006-pure-engine-shared-with-replay.md) | A pure decision engine shared by the controller and replay | Accepted |
| [0007](0007-minimal-dependencies.md) | Minimal dependencies | Accepted |
| [0008](0008-moby-client-and-go-1-26.md) | moby/moby/client SDK and Go 1.26 | Accepted |
| [0009](0009-recordings-store-totals.md) | Recordings store service-wide totals | Accepted |
| [0010](0010-runtime-memory-as-files.md) | Runtime memory as plain files | Accepted |
| [0011](0011-wall-clock-aligned-seasonality.md) | Seasonality aligned to the wall clock | Accepted |
| [0012](0012-grade-decisions-by-outcome.md) | Grade every decision by its outcome | Accepted |
| [0013](0013-prewarm-as-a-policy-signal.md) | History prewarm as an ordinary policy signal | Accepted |
| [0014](0014-embedded-read-only-ui.md) | An embedded, dependency-free, read-only web UI | Accepted |

## Adding a record

1. Copy [template.md](template.md) to `NNNN-short-title.md` with the next number.
2. Fill it in. Keep it short: the reasoning matters more than the details.
3. Add it to the table above.
4. Never edit an accepted decision's reasoning after the fact. If it changes,
   write a new record and mark the old one `Superseded by NNNN`.
