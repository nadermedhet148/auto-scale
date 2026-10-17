# 0014. An embedded, dependency-free, read-only web UI

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

Operators need to see what dockscale is doing: replica counts over time, the
signals behind each decision, how past decisions turned out, and what the load
profile has learned. JSON endpoints and a CLI exist, but they don't show
trends.

## Decision

- Serve a dashboard **from the dockscale binary itself**, at `/ui/`, with static
  files embedded via `go:embed` (`internal/ui`).
- Write it in **plain HTML, CSS and JavaScript**: no framework, no build step,
  no CDN. The charts are a small SVG line-chart component in `app.js`.
- Make it **read-only**: it polls `/api/*` JSON endpoints, and there are no
  endpoints that change anything.
- The controller keeps a **3-hour ring of ticks** per service for charting,
  and publishes a copy of the load profile after each tick, so HTTP handlers
  never touch engine state.
- `/api/overview` exposes a **sanitized** view of the config. Image, env,
  labels and connection URLs are excluded, because URLs can hold credentials.
- Ship `cmd/dockscale-demo`: the real controller against a fake Docker with
  synthetic load, so the UI can be used and developed without Docker.

## Alternatives considered

- **A React or Vue app with a Node build:** a richer toolkit, but it adds a
  toolchain and hundreds of dependencies to a project that has three (ADR
  0007).
- **Grafana over `/metrics`:** good for long-term metrics, but it can't show
  decisions, verdicts, reasons or the learned profile without extra plumbing,
  and it's another service to run. It remains a complement, not a
  replacement.
- **Control actions (manual scale, pause):** useful, but they'd turn an
  unauthenticated port into a remote-control surface for containers. Deferred
  until there's authentication.

## Consequences

- The binary grows by about 40 KB (the three static files), there's nothing to install, and it works
  offline.
- The chart component is code of our own to maintain. It's about 300 lines,
  covering hover, keyboard navigation, the table view and the legend.
- Chart history lives only in memory, for 3 hours. For longer history, use
  Prometheus via `/metrics`, or the journal.
- Building the demo surfaced two real bugs that the unit tests had missed: a
  Windows file-replace race in state saving (now retried) and oversized
  indented `/api` JSON. It's worth keeping the demo working.
