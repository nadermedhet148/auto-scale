# 0013. History prewarm as an ordinary policy signal

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

Reactive scaling always lags a sudden jump by the replicas' start-up time.
Many services have recurring jumps (business hours, scheduled jobs). The
learned load profile knows them in advance.

## Decision

With `history.prewarm`, for each additive metric the engine adds a signal
`metric@history` whose value is
`max(profile(now), profile(now + lead)) / current`. Here `profile` is the
slot's mean + `sigma`·stddev, used only once the slot has `min_samples`
visits. The policy treats it like any other signal: the largest recommendation
wins, and cooldowns, step limits and stabilization still apply.

## Alternatives considered

- **A dynamic `min` (floor) from history:** it would bypass cooldowns and
  stabilization, and make prewarm a special case in the policy.
- **Scheduled scaling rules written by hand:** they need maintenance and
  don't adapt.

## Consequences

- No special cases in the policy. Prewarm shows up in reasons and in `/status`
  like any signal.
- Measured with replay on 4 days of business-hours load: the 09:00 overload
  went from 9 ticks a day to 0 from day 3, for +0.5% replica-hours. On a smooth
  daily ramp it made no difference, because Holt already tracks gradual change.
- During usual busy hours it keeps capacity even on a quiet day. That's the
  trade-off.
- Off by default. It needs `min_samples` cycles of history before it acts.
