# 0009. Recordings store service-wide totals

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

`dockscale -record` captures live load for replay. Replay runs a *different*
policy, so it has a different replica count from the one that was recorded.

## Decision

Recordings store **service-wide totals**: summed CPU %, total rps, total
backlog. The simulator divides them by its own count of *ready* replicas.
Latency isn't additive, so it's stored and replayed as observed. The
simulator models a start-up delay: a new replica counts for the policy at once,
but only takes load after `-startup-delay`.

## Alternatives considered

- **Per-replica values:** they only make sense at the recorded replica count.

## Consequences

- Assumes load divides evenly across replicas: close for CPU, rps and backlog
  behind a load balancer, wrong for latency.
- Assumes demand doesn't depend on capacity (no retries, no users leaving).
- The load profile (0010) learns totals for the same reason.
