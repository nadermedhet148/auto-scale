# 0004. Target tracking with fast scale-up and slow scale-down

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

Signals from different metrics (CPU %, rps, latency, backlog) have to be
combined into one replica count, and the count must not flap.

## Decision

- **Target tracking**, as in the Kubernetes HPA: for each metric,
  `n = ceil(current × value / target)`. The largest `n` wins, clamped to
  `[min, max]`. A ±`tolerance` band around the target means "no change".
- **Asymmetric behaviour:**
  - Scale-up waits only for a short cooldown (30s) and adds up to 4 replicas
    per step.
  - Scale-down shrinks only to the highest recommendation of the last 5
    minutes (the stabilization window), waits 2 minutes after any action, and
    removes 1 replica per step.
- The controller commits the replica count it actually **reached**, not the one
  it wanted, so failed starts still trigger the cooldown.

## Alternatives considered

- **Fixed thresholds** ("CPU > 80% → +1"): slow to converge, and they don't
  compose across metrics.
- **PID control:** harder to tune and explain, with little benefit at a 10s
  interval.

## Consequences

- Being short of capacity costs errors; extra capacity only costs money. The
  asymmetry reflects that.
- Latency doesn't fall in proportion to replica count, so latency-driven
  scaling relies on `max_step_up` to limit overshoot.
