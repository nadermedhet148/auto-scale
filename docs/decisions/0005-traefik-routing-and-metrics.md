# 0005. Traefik for routing and request metrics

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

Scaled replicas need traffic spread across them, and scaling on request rate
and latency needs per-service request metrics.

## Decision

Use **Traefik** with its Docker provider. Replicas carry `traefik.*` labels,
copied from the service template, and Traefik discovers them by itself.
dockscale scrapes Traefik's Prometheus endpoint for
`traefik_service_requests_total` and
`traefik_service_request_duration_seconds_bucket`, and works out rps and p95
from the difference between scrapes.

## Alternatives considered

- **A reverse proxy built into dockscale:** full control, but much more code,
  and dockscale would sit in the data path.
- **Nginx with config reloads:** dockscale would have to template and reload
  the config.

## Consequences

- dockscale and the data path are fully separate: if dockscale is down,
  traffic still flows.
- Depends on Traefik's metric names, and its default latency buckets (0.1,
  0.3, 1.2, 5s) are too coarse for p95. The demo sets finer buckets.
- Non-HTTP workers don't need Traefik (queue_depth, cpu).
