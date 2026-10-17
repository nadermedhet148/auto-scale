# dockscale documentation

| Document | Read it to learn |
|---|---|
| [Architecture](architecture.md) | The components, package layout, dependencies and the main design decisions |
| [Control loop flow](control-loop.md) | What happens on every tick, step by step, including failure handling |
| [Metrics](metrics.md) | Where each signal comes from and how it is turned into a per-replica value |
| [Statistical models](models.md) | The maths behind the outlier filter, EWMA, Holt and Holt-Winters, plus a tuning guide |
| [Scaling policy](policy.md) | How a replica count is chosen: target tracking, tolerance, cooldowns, stabilization |
| [Replay and tuning](replay.md) | Recording load, simulating policies offline and reading the results |
| [Operations](operations.md) | Deploying, the full configuration reference, endpoints, failure modes and limits |

New to the project? Read them in this order: architecture → control loop → policy.
The rest are reference.
