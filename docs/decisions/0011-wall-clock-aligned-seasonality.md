# 0011. Seasonality aligned to the wall clock

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

Holt-Winters originally indexed its seasonal slots with a sample counter
(`i = n mod P`). That only works with no gaps. Once state is persisted
(0010), any downtime would shift the whole season: after a 1,003-tick gap with
an 8-slot season, every slot would be off by 3.

## Decision

Seasonal slots, and load-profile slots, are computed from wall-clock time:
`slot = (unix_nanos mod season_length) / interval`, in UTC. The `Estimator`
interface takes the sample's timestamp: `Update(t, x)`. Holt-Winters
initializes after one cycle's worth of samples, and slots it never saw start
at 0.

## Alternatives considered

- **Keep the counter and persist it with the gap length added:** fragile, and
  it breaks on clock adjustments and skipped ticks.

## Consequences

- Seasons survive gaps and restarts. `TestHoltWintersSeasonSurvivesGap` covers
  this.
- Slots are in UTC. With a 24h cycle, a daylight-saving shift moves local
  peaks by an hour against the profile. It heals over a few days through
  `decay`/`gamma`.
- Holt-Winters went from 15 to 19 ns per sample, which is negligible.
