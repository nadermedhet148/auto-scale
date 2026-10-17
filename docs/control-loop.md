# Control loop flow

Everything dockscale does happens in one loop. It runs once at startup and then
every `interval` (default 10s). Each iteration is called a **tick**.

## Startup

```mermaid
sequenceDiagram
    participant main
    participant config
    participant docker as docker.Client
    participant ctl as Controller
    participant http as HTTP server

    main->>config: Load(path)
    Note over config: parse YAML (unknown keys rejected)<br/>apply defaults<br/>validate, derive Holt-Winters period
    main->>docker: New() and Ping()
    Note over docker: DOCKER_HOST or platform default,<br/>API version negotiated
    opt -record given
        main->>main: open recording CSV
    end
    opt memory.dir set
        main->>main: open memory store
    end
    main->>ctl: New(cfg, docker, recorder, memory)
    Note over ctl: one Engine per service,<br/>Traefik scraper and queue source if used
    opt memory store
        ctl->>ctl: restore state-<service>.json into each Engine<br/>load recent journal entries for /history
    end
    main->>http: ListenAndServe(listen)
    main->>ctl: Run(ctx), blocks until SIGINT or SIGTERM
    ctl-->>main: ctx cancelled
    main->>ctl: Close(): save every service's learned state
    main->>http: Shutdown (5s)
    Note over main: replicas are left running,<br/>the next start adopts them
```

## One tick

Each service is handled independently and in parallel. This is the sequence for
one service:

```mermaid
sequenceDiagram
    autonumber
    participant ctl as Controller.tick
    participant dk as Docker
    participant src as Traefik / Queue
    participant eng as Engine
    participant rec as Recorder

    ctl->>dk: Replicas(service)
    dk-->>ctl: live (oldest first), dead
    loop each dead replica
        ctl->>dk: Remove(id, grace 0)
    end
    par collect signals
        ctl->>dk: Usage(live ids), CPU % and memory %
    and
        ctl->>src: Scrape / Depth
    end
    Note over ctl: convert everything to per-replica values
    opt recording
        ctl->>rec: Record(service-wide totals)
    end
    ctl->>eng: Step(now, current, perReplica)
    Note over eng: per metric: outlier filter, then estimator<br/>then policy.Decide
    eng-->>ctl: signals, Decision{Recommended, Desired, Reason}
    Note over ctl: grade decisions whose evaluation window ended<br/>snapshot state if snapshot_every has passed
    alt Desired == current
        Note over ctl: hold (logged at debug level)
    else dry_run
        ctl->>eng: Commit(now, current, desired)
        Note over ctl: log only, containers untouched
    else scale
        ctl->>dk: Start × n  or  Remove(newest) × n, in parallel
        dk-->>ctl: replicas actually reached
        ctl->>eng: Commit(now, current, reached)
    end
    opt an action was taken (or would be, in dry run)
        ctl->>ctl: journal the decision, start watching its outcome
    end
    Note over ctl: publish Status for /status and /metrics
```

### Step by step

1. **Observe.** List all containers labelled `dockscale.managed=true` and
   `dockscale.service=<name>`. States `running`, `restarting` and `paused` count
   as live. Anything else (`exited`, `dead`, `created`) is dead.
2. **Clean up.** Remove dead replicas. Replicas use the `unless-stopped`
   restart policy, so a crashing container is restarted by Docker itself. A
   container is only dead here if something stopped it from outside. Removing
   it lets the next step replace it.
3. **Collect.** Read each configured metric and normalize it to a
   **per-replica** value (see [metrics.md](metrics.md)). A metric that can't be
   read this tick is simply left out. It doesn't block the others, and it
   doesn't count as zero.
4. **Record** (optional). Append the service-wide totals to the recording CSV
   for later replay (see [replay.md](replay.md)).
5. **Estimate.** Each metric's value goes through its own pipeline: outlier
   filter, then estimator (see [models.md](models.md)). The service-wide totals
   also update the load profile, and with `history.prewarm` it adds
   `metric@history` signals (see [memory.md](memory.md)).
6. **Decide.** The policy turns the estimates into a desired replica count (see
   [policy.md](policy.md)).
7. **Act.** Start or remove the difference in parallel. Scale-down removes the
   **newest** replicas first.
8. **Commit.** Tell the policy what actually happened, which starts the
   cooldowns. dockscale commits the replica count it actually *reached*, not the
   one it asked for.
9. **Remember.** Grade earlier decisions whose `evaluate_after` window has
   ended, journal this tick's decision if there was one, and save a state
   snapshot every `snapshot_every`.
10. **Publish.** Store the `Status` (replicas, recommendation, signals, reason,
   last error) for the HTTP endpoints.

## Decision flow

```mermaid
flowchart TD
    start([perReplica values]) --> bounds{current outside<br/>min..max?}
    bounds -->|below min| tomin[desired = min]
    bounds -->|above max| tomax[desired = max]
    bounds -->|no| sig{any signals?}
    sig -->|no| hold1[hold: no signals]
    sig -->|yes| rec["per metric:<br/>n = ceil(current × value / target)<br/>or current if within ±tolerance<br/>rec = clamp(max over metrics)"]
    rec --> dir{rec vs current}
    dir -->|equal| hold2[hold: steady]
    dir -->|higher| upcd{scale-up cooldown<br/>elapsed?}
    upcd -->|no| hold3[hold: cooldown]
    upcd -->|yes| up["desired = min(rec, current + max_step_up)"]
    dir -->|lower| stab["stable = max rec seen in<br/>scale_down_stabilization window"]
    stab --> stabq{stable < current?}
    stabq -->|no| hold4[hold: stabilizing]
    stabq -->|yes| dncd{scale-down cooldown<br/>elapsed since last scale<br/>in either direction?}
    dncd -->|no| hold5[hold: cooldown]
    dncd -->|yes| down["desired = max(stable, current − max_step_down)"]
```

Every branch sets a human-readable `Reason`. It's logged and shown in
`/status`, so any decision can be explained after the fact:

```
level=INFO msg=scale service=web from=2 to=4 reason="cpu=41.2/60.0->2 rps=97.3/50.0->4 latency_p95=88.0/200.0->1"
```

Each `metric=value/target->n` entry shows the smoothed value, the target, and
the replica count that metric alone would ask for. The largest wins.

## Scale-up flow

```mermaid
sequenceDiagram
    participant ctl as Controller
    participant dk as docker.Client
    participant eng as Docker Engine
    participant tr as Traefik

    ctl->>dk: Start(svc) × n (parallel)
    dk->>eng: POST /containers/create<br/>(image, env, labels, cpus, memory, network)
    alt image not found (404)
        dk->>eng: POST /images/create (pull)
        dk->>eng: POST /containers/create (retry once)
    end
    dk->>eng: POST /containers/{id}/start
    alt start fails
        dk->>eng: DELETE /containers/{id} (no orphans left)
    end
    eng-->>tr: container event
    Note over tr: discovers it via traefik.* labels<br/>and starts routing (honouring<br/>healthchecks if the image has one)
```

Replica names are `<service>-<6 random hex chars>`. dockscale adds two labels
of its own (`dockscale.managed=true`, `dockscale.service=<name>`) to the
template's labels.

## Scale-down flow

```mermaid
sequenceDiagram
    participant ctl as Controller
    participant eng as Docker Engine
    participant tr as Traefik

    Note over ctl: victims = live[desired:]<br/>(live is sorted oldest first,<br/>so these are the newest)
    ctl->>eng: POST /containers/{id}/stop?t=stop_timeout (parallel)
    Note over eng: SIGTERM, then SIGKILL<br/>after stop_timeout
    eng-->>tr: container stopped
    Note over tr: removes it from the pool
    ctl->>eng: DELETE /containers/{id}?force=1
```

## Failure handling

| Failure | What happens |
|---|---|
| Docker API unreachable when listing | Tick aborted, `LastError` set, no scaling. Retried next tick. |
| Stats fail for some replicas | Those replicas are left out of the average and a warning is logged. If none succeed, the CPU and memory signals are skipped. |
| Traefik or queue unreachable | Those signals are skipped this tick. Scaling continues on the remaining signals. With none left, the policy holds. |
| Only some starts succeed | dockscale commits the count it reached, so the scale-up cooldown starts and there's no retry storm. The error is logged and surfaced. It retries after the cooldown. |
| Image pull fails | Counts as a failed start (as above). |
| A replica is removed by hand | It no longer appears in the list, so it's replaced on the next tick if the policy wants it. |
| dockscale crashes or restarts | Replicas keep running and are adopted by label. With `memory.dir`, learned state is restored from the last snapshot (at most `snapshot_every` old after a crash). Without it, the estimators warm up again and Holt-Winters relearns its season. |
| Memory directory not writable | Startup fails if it can't be opened. Later write failures are logged as warnings and scaling continues. |
| A tick is slower than the interval | At most one missed tick is kept, the rest are dropped. Observation uses a timeout of one interval; scaling actions get up to 3 minutes. |
| Stopped (not crashed) replica | Removed during cleanup, then replaced if still needed. |

## Timeline example

`interval: 10s`, `scale_up_cooldown: 30s`, `scale_down_cooldown: 2m`,
`scale_down_stabilization: 5m`, rps target 50 per replica:

```
t=0s     2 replicas, 200 rps total → 100/replica → rec 4  → scale 2→4
t=10s    4 replicas, 50/replica    → steady
t=40s    load 400 rps → 100/replica → rec 8; cooldown ended → scale 4→8
t=3m     load drops to 100 rps → 12.5/replica → rec 2
         stabilization window still holds rec 8 → hold ("stabilizing")
t=8m     window has aged out the high recommendations → stable 2
         cooldown elapsed → scale 8→7 (max_step_down 1)
t=10m    → 7→6 … continues one replica every 2m down to 2
```

Scale-up is quick and scale-down is slow and gradual. That asymmetry is
deliberate: being short of capacity costs errors, while extra capacity only
costs money.
