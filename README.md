# dockscale

> Full documentation: **[docs/](docs/README.md)**: [architecture](docs/architecture.md),
> [control loop](docs/control-loop.md), [metrics](docs/metrics.md),
> [models](docs/models.md), [policy](docs/policy.md), [replay](docs/replay.md),
> [operations](docs/operations.md).

A horizontal autoscaler for plain Docker on one host. It watches a service's
replicas, estimates load with simple statistical models, and starts or stops
containers to keep each metric near its per-replica target. Traefik spreads
traffic across the replicas.

```
 every interval (10s)
 ┌──────────────────────────────────────────────────────────────────────┐
 │ observe     list containers labelled dockscale.service=<name>         │
 │ collect     docker stats  → cpu %, memory %       (per replica avg)   │
 │             traefik /metrics → rps per replica, p95 latency           │
 │             redis / rabbitmq → queue backlog per replica              │
 │ estimate    MAD outlier clamp → EWMA / Holt / Holt-Winters forecast   │
 │ decide      target tracking → tolerance → cooldown → stabilization    │
 │ act         docker create/start  or  stop/remove (newest first)       │
 └──────────────────────────────────────────────────────────────────────┘
```

## How a decision is made

1. **Outlier filter.** A sample further than `k·MAD` from the rolling median
   gets clamped, so one bad scrape can't trigger scaling. The window holds raw
   samples, so a real, sustained shift takes over the median after about
   `outlier_window/2` ticks.
2. **Estimator.**
   - `ewma`: `s = α·x + (1-α)·s`. Smooths noise.
   - `holt`: double exponential smoothing with level and trend. It returns a
     forecast `horizon` ticks ahead, so rising load scales out before it peaks.
   - `holt_winters`: Holt plus a learned seasonal offset for every point in
     a cycle (`season_length`, e.g. `24h`). It scales out ahead of the daily
     peak because it remembers yesterday's. The first cycle is spent learning,
     and it behaves like `holt` until then.
   - `raw`: no smoothing.
3. **Target tracking** (same formula as Kubernetes HPA), per metric:
   `desired = ceil(current × estimate / target)`. A ratio within ±`tolerance`
   counts as steady. The largest result across metrics wins, clamped to
   `[min, max]`.
4. **Scale up** is fast: it waits only for `scale_up_cooldown` and adds at most
   `max_step_up` replicas per step.
5. **Scale down** is slow: it shrinks only to the *highest* recommendation in
   the last `scale_down_stabilization` window, waits `scale_down_cooldown`
   after any scaling action, and removes at most `max_step_down` per step.

Each decision is logged with its reasoning, for example:

```
level=INFO msg=scale service=web from=2 to=4 reason="cpu=41.2/60.0->2 rps=97.3/50.0->4 latency_p95=88.0/200.0->1"
```

## Run the demo

Requires Docker (Docker Desktop on Windows/macOS works).

```sh
docker compose up --build -d
docker compose logs -f dockscale        # watch decisions
curl localhost:9090/status              # current state per service
```

dockscale creates the 2 `min` replicas of `traefik/whoami`. To generate load:

```sh
docker run --rm --network dockscale_net williamyeh/hey -z 3m -c 50 http://traefik/
```

The rps signal climbs past 50/replica and the service scales out. After the
load stops, it scales back in one replica at a time, once the stabilization
window has passed.

To tear down, remove the stack and then the replicas, which compose doesn't own:

```sh
docker compose down
docker rm -f $(docker ps -aq --filter label=dockscale.managed=true)
```

## Tune offline with replay

`dockscale-replay` runs recorded load through the same decision engine the
controller uses, so you can compare policies without touching Docker.

```sh
# 1. Get a recording: record production...
dockscale -config dockscale.yaml -record load.csv
# ...or synthesise a few days of daily-cycle traffic with spikes
go run ./cmd/dockscale-replay generate -out load.csv -days 3 -base 20 -peak 400

# 2. Compare configs on identical load
go run ./cmd/dockscale-replay -in load.csv   -config examples/policies/raw.yaml -config examples/policies/ewma.yaml   -config examples/policies/holt.yaml -config examples/policies/holt_winters.yaml   -timeline timeline.csv
```

On the synthetic 3-day trace:

```
config        model         scale-ups  scale-downs  peak  replica-h  overload %
raw.yaml      raw                  53           88    14      386.9        0.69
ewma.yaml     ewma                 52           76    12      370.9        1.33
holt.yaml     holt                 66          113    14      395.1        0.65
holt_winters  holt_winters         93          178    15      428.2        0.58
```

The trade-off is visible: EWMA is cheapest but lags behind spikes, while
Holt-Winters overloads least but spends more replica-hours and scales more
often. Lower `gamma` or a larger `tolerance` reduces the churn.

The two numbers to tune against:

- **replica-h**: cost.
- **overload**: time when some metric per *ready* replica exceeded
  `target × (1 + tolerance)`. `-startup-delay` (default 10s) models how long
  a new replica takes before it serves load.

Recordings store service-wide totals (summed CPU %, total rps, total queue
backlog), so the simulator can split the same load across a different replica
count. That assumes load divides evenly across replicas, which holds for CPU
and rps but not for latency, which is replayed as recorded. `-timeline`
writes per-tick replicas, raw and estimated signals, and the reason for each
decision.

## Run natively

Requires Go 1.26+.

```sh
go test ./...
go run ./cmd/dockscale -config examples/dockscale.yaml -dry-run -v
```

`-dry-run` logs decisions without touching containers. Outside compose,
change `prometheus.url` to an address you can reach, such as
`http://localhost:8082/metrics`, after publishing that port on Traefik.

## Testing

```sh
go test ./...                                        # unit + integration tests
go test -run '^$' -bench . ./internal/...            # benchmarks
go test -run '^$' -fuzz FuzzParse ./internal/metrics # fuzz the metrics parser
```

The Docker wrapper is tested against an in-memory fake of the Engine API
(`internal/docker/docker_test.go`). The controller is tested through a fake
`Docker` interface with an injected clock, so cooldown behavior can be checked
tick by tick.

## Configuration

See [examples/dockscale.yaml](examples/dockscale.yaml). Metrics:

| type          | source              | meaning of `target`                                    |
|---------------|---------------------|--------------------------------------------------------|
| `cpu`         | Docker stats        | % of `template.cpus` per replica (of 1 core if unset)  |
| `memory`      | Docker stats        | % of the memory limit per replica                      |
| `rps`         | Traefik `/metrics`  | requests/sec per replica                               |
| `latency_p95` | Traefik `/metrics`  | p95 latency in ms (from histogram buckets)             |
| `queue_depth` | Redis list / RabbitMQ | pending messages per replica                         |

For queue workers, see [examples/worker.yaml](examples/worker.yaml). Redis
uses `LLEN` on a list key (with AUTH/SELECT from the URL). RabbitMQ uses the
management API's `messages` count, which is ready plus unacked.

## Endpoints

- `GET /status`: JSON per service with replicas, recommendation, desired count, signals and last reason
- `GET /metrics`: `dockscale_replicas`, `dockscale_desired_replicas`, `dockscale_signal{metric,kind=raw|estimate|target}`
- `GET /healthz`

## Behaviour notes

- dockscale only touches containers labelled `dockscale.managed=true`. When it
  restarts, it picks up existing replicas instead of creating new ones.
- Stopped replicas are removed and replaced on the next tick.
- Scale-down sends SIGTERM and waits `stop_timeout`. Traefik drops the replica
  as soon as it stops, but requests already in flight depend on the app
  shutting down gracefully.
- CPU needs two samples per container, so a new replica contributes CPU data
  from its second tick onwards.
- Latency doesn't fall in proportion to replica count, so using
  `latency_p95` together with `max_step_up` keeps overshoot in check.

## Roadmap

- Persist model state across restarts. Holt-Winters currently relearns a full season after every restart.
- Redis streams / consumer-group lag as a queue source
- Scale to zero, with activation on first request
- Docker Swarm / multi-host backend behind the same engine
