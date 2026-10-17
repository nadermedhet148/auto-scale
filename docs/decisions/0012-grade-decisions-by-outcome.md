# 0012. Grade every decision by its outcome

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

To learn from the past, dockscale needs to know not only *what* it decided but
*whether it was right*. That has to be judged automatically and consistently,
both live and in replay.

## Decision

`memory.Evaluator` watches each non-dry-run decision for `evaluate_after`
(default 10m), counts ticks where a live metric is above
`target × (1 + tolerance)`, and assigns a verdict:

- **Scale-up:** `unnecessary` if it was undone (the count fell back to ≤ its
  starting count) within the window; `insufficient` if overloaded > 50% of the
  window; otherwise `effective`.
- **Scale-down:** `premature` if it was undone (the count rose back to ≥ its
  starting count) or overloaded > 20% of the window; otherwise `ok`.

History (`@history`) signals are forecasts, so they never count as load.
`dockscale-memory report` turns verdict ratios into tuning advice. It never
changes the config by itself.

An earlier draft marked a decision as reversed whenever *any* later decision
went the other way. Replay showed that this blamed the earlier scale-ups in
"2→3, 3→5, 5→4", even though the count never returned to 2 or 3. That draft
was replaced before release with the "actually undone" rule.

## Alternatives considered

- **Automatically adjusting parameters from verdicts:** a feedback loop on the
  autoscaler itself is risky. It's deferred until the advice has proven
  reliable.
- **Grading by cost (replica-hours):** needs a counterfactual, which only
  replay can provide.

## Consequences

- Verdicts are heuristic. "Unnecessary" can't tell a noise spike from a brief
  real burst, so use them as trends, not individual judgments.
- Advice needs at least 5 graded decisions before it says anything.
