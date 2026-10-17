# 0006. A pure decision engine shared by the controller and replay

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

Policies need tuning offline against recorded traffic. An offline simulator
that reimplemented the decision logic would drift from production and give
misleading results.

## Decision

- All decision logic lives in `internal/engine` (models, profile and policy).
  It's **pure**: no I/O, no goroutines, no `time.Now()`. Time is passed in.
- `internal/controller` (live) and `internal/replay` (offline) both drive the
  **same** `engine.Engine`.
- `internal/model`, `internal/policy` and `internal/memory` (except
  `store.go`) follow the same rule.

## Alternatives considered

- **A separate simulator:** quick to write, but it drifts from production.
- **Replaying against a fake Docker through the real controller:** possible,
  but slow (I/O, goroutines) and harder to make deterministic.

## Consequences

- Replay is trustworthy and fast: 3 days of 10s ticks in about 30–60 ms.
- Core logic is unit-testable with fixed timestamps.
- The engine can't fetch anything itself. The controller gathers inputs and
  passes them in.
- Rule for contributors: **never add I/O or `time.Now()` to the pure
  packages.**
