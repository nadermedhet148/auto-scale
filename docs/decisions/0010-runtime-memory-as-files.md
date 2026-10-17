# 0010. Runtime memory as plain files

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

The project needed "memory about what happened, what the decision was, and a
way to use them for future cases". Before this, all learned state was lost on
restart. Holt-Winters in particular spent a full season (a day) relearning,
and nothing recorded whether past decisions had been good.

## Decision

With `memory.dir` set, dockscale keeps:

- `state-<service>.json`: the engine's learned state (models, policy
  cooldowns and stabilization history, load profile). It's saved every
  `snapshot_every` and on shutdown, written to a temp file and renamed into
  place. On restore:
  - **short-term** state (outlier window, EWMA value, Holt level and trend) is
    used only if the snapshot is fresh (≤ `max_state_age`);
  - **long-term** state (seasonal offsets, load profile, cooldown times) is
    always used.
- `journal.jsonl`: an append-only log of decisions and their graded outcomes
  (see 0012).

Without `memory.dir`, behaviour is unchanged and nothing is written.

## Alternatives considered

- **SQLite:** good for querying, but needs cgo or a large pure-Go port.
- **bbolt:** small and pure Go, but still a dependency for data we only append
  to and read back in full.
- **Prometheus as the history store:** it holds metrics, not decisions and
  model state, and adds an operational dependency.

## Consequences

- Zero new dependencies. The files can be read with `jq` and `cat`.
- One writer per directory. Two instances sharing it would corrupt each other.
- The journal isn't rotated (about 0.2 MB/day at 200 actions/day). Rotation is
  on the roadmap.
- Each restore part is checked against the config. Parts that no longer match
  (a model kind or season length changed) are skipped with a log note, instead
  of being misapplied.
