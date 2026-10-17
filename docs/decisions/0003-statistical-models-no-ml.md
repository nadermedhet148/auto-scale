# 0003. Statistical models, no ML

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

The original requirement was explicit: "no LLM or AI for now, maybe
statistical models". Scaling decisions have to be predictable and explainable,
and work from the first minute without training data.

## Decision

Each metric goes through a pipeline of classical time-series statistics:

1. A robust outlier filter: rolling median ± k·MAD.
2. One estimator: raw, EWMA, Holt (level + trend), or additive Holt-Winters
   (level + trend + season).

Learning from the past (see 0010–0013) uses the same kind of statistics:
exponentially weighted per-slot means and variances, and threshold rules for
grading.

## Alternatives considered

- **ML forecasters (gradient boosting, LSTM):** they need training data and
  infrastructure, are opaque, and were out of scope by requirement.
- **No smoothing (raw thresholds):** flaps on noise.

## Consequences

- Every decision can be explained by a formula and logged as a reason string.
- Cheap: about 2 µs per service per tick.
- Can't learn complex non-periodic patterns. That's the reactive signals' job,
  and the reason `dockscale-replay` exists for comparing models.
- Parameters (`alpha`, `beta`, `gamma`, `horizon`) need tuning. Replay and the
  memory report support that.
