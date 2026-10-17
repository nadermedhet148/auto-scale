# Scaling policy

The policy (`internal/policy`) turns metric estimates into a replica count. It's
pure: time is passed in, and there's no I/O. It holds only three pieces of
state: the time of the last scale-up, the time of the last scale-down, and a
short history of recommendations.

## 1. Target tracking

For each metric:

```
ratio = estimate / target
n     = current                          if |ratio − 1| ≤ tolerance
        ceil(current × ratio)            otherwise
```

The recommendation is the **largest `n` across all metrics**, clamped to
`[min, max]`. (A tiny epsilon is subtracted before `ceil`, so floating-point
noise like `3.0000000001` doesn't round up to 4.)

Example, with 4 replicas:

| metric | estimate | target | ratio | n |
|---|---|---|---|---|
| cpu | 45 | 60 | 0.75 | 3 |
| rps | 70 | 50 | 1.40 | 6 |
| latency_p95 | 190 | 200 | 0.95 (within ±10%) | 4 |

Recommendation: **6**. The rps metric decides here.

## 2. Tolerance band

`tolerance` (default 0.1) creates a dead zone of ±10% around each target, where
the metric asks for no change. Without it, a value that wobbles between 59% and
61% CPU would make the replica count wobble too.

## 3. Hard bounds

If the *current* count is outside `[min, max]`, for example at first start with
0 replicas, or after someone lowered `max`, the policy returns the bound
straight away and ignores cooldowns. Bounds are a correctness rule, not
something to smooth.

## 4. Scale-up rules

Scale-up is quick:

- It's allowed once `scale_up_cooldown` (default 30s) has passed since the last
  scale-up. That gives the new replicas time to start taking load before more
  are added.
- Each step adds at most `max_step_up` replicas (default 4). This limits
  overshoot when a spike or a bad estimate asks for many replicas at once.

## 5. Scale-down rules

Scale-down is deliberately slow, using three mechanisms.

**Stabilization window.** The policy remembers every recommendation from the
last `scale_down_stabilization` (default 5m) and shrinks only to the **highest**
of them:

```
stable = max(recommendations in the last window)
```

A short lull in the middle of a busy period never removes capacity that would
be needed again a minute later. Kubernetes uses the same mechanism.

```
time   rec    stable (5m window)   action
0:00    8          8               (at 8)
0:30    3          8               hold, stabilizing
2:00    3          8               hold, stabilizing
5:10    3          3               the high recs have aged out → may shrink
```

**Cooldown.** Scale-down waits for `scale_down_cooldown` (default 2m) since the
last scaling action **in either direction**. This means it never removes
replicas just after adding them.

**Step limit.** Each step removes at most `max_step_down` replicas (default 1),
so shrinking is gradual and can be stopped part-way if load comes back.

## 6. Commit

The controller calls `Commit(now, from, to)` with the replica count it actually
reached, and that starts the matching cooldown. Committing the *reached* count
rather than the requested one matters: if only 2 of 4 starts succeed, the
cooldown still starts, so a failing image pull doesn't trigger new attempts
every tick.

## Behavior settings

| Setting | Default | Raise it to… | Lower it to… |
|---|---|---|---|
| `tolerance` | 0.1 | scale less often | track targets more tightly |
| `scale_up_cooldown` | 30s | avoid overshoot with slow-starting replicas | react faster to sustained growth |
| `scale_down_cooldown` | 2m | keep capacity longer | save cost sooner |
| `scale_down_stabilization` | 5m | ride out lulls | release capacity after a spike sooner |
| `max_step_up` | 4 | absorb sudden large spikes | limit overshoot |
| `max_step_down` | 1 | shrink faster | shrink more cautiously |

**Rule of thumb:** set `scale_up_cooldown` to at least your replica start-up
time, so the policy sees the effect of the last scale-up before deciding again.
Set `scale_down_stabilization` longer than the typical gap between bursts.

## Reasons

Every decision has a reason string, prefixed by why the count was held:

| Prefix | Meaning |
|---|---|
| `below min N` / `above max N` | A hard bound was applied |
| `no signals` | No metric could be read this tick, so the count is held |
| `steady;` | Recommendation equals the current count |
| `scale-up cooldown;` | Wanted more replicas, but still in the cooldown |
| `scale-down stabilizing (window max N);` | Wanted fewer, but recently recommended N |
| `scale-down cooldown;` | Wanted fewer, but still in the cooldown |
| *(no prefix)* | The action was taken |

The rest of the string lists `metric=estimate/target->n` for each metric.
