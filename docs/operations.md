# Operations

## Deployment

dockscale needs access to the Docker Engine API on the host it manages.

**In a container** (recommended), as in the demo `docker-compose.yml`:

```yaml
dockscale:
  build: .
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock
    - ./dockscale.yaml:/etc/dockscale/dockscale.yaml:ro
  networks: [dockscale_net]   # the same network as Traefik and the replicas
```

**Natively** (Go 1.26+):

```sh
go build -o dockscale ./cmd/dockscale
./dockscale -config dockscale.yaml
```

The Docker connection comes from the environment: `DOCKER_HOST`,
`DOCKER_TLS_VERIFY`, `DOCKER_CERT_PATH`, otherwise the platform default
(`/var/run/docker.sock`, or the named pipe on Windows). The API version is
negotiated, and versions 1.40 through 1.56 are supported.

### Networking requirements

- Replicas must join a network Traefik can reach (`template.network`), and
  Traefik must be told to use it (`--providers.docker.network=…`).
- dockscale must be able to reach `prometheus.url` (Traefik's metrics entry
  point) and `queue.url`.
- The image runs as root, which it needs for socket access, on
  `gcr.io/distroless/static-debian12`.

## Command-line flags

### `dockscale`

| Flag | Default | Description |
|---|---|---|
| `-config` | `dockscale.yaml` | Config file path |
| `-dry-run` | false | Log decisions, change nothing. Overrides the config's `dry_run`. |
| `-record` | (none) | Append each tick's load to this CSV for replay |
| `-v` | false | Debug logging, including every hold decision |

Memory is configured in the file (`memory.dir`), not with a flag.

### `dockscale-replay`

| Flag | Default | Description |
|---|---|---|
| `-in` | `load.csv` | Recording to replay |
| `-config` | `examples/dockscale.yaml` | Config to evaluate. Repeat it to compare several. |
| `-service` | first in config | Service to replay |
| `-start` | `min` | Initial replicas |
| `-startup-delay` | 10s | Time before a new replica takes load |
| `-timeline` | (none) | Write a per-tick CSV for the first config |
| `-memory-dir` | (none) | Write the first config's journal and learned state here |

`dockscale-replay generate` takes `-out -service -days -interval -shape (sine|step) -base -peak -noise -spike-p -cpu-per-rps -seed`.

### `dockscale-memory`

| Subcommand | Flags | Shows |
|---|---|---|
| `decisions` | `-dir -service -since -limit -json` | Past decisions with verdicts, in UTC |
| `report` | `-dir -config -service -since -json` | Verdict counts and tuning advice per service |
| `profile` | `-dir -service -metric` | The learned load profile, slot by slot |

`-since` counts back from the newest journal entry.

## Configuration reference

Unknown keys are rejected, so a typo fails at startup instead of being silently
ignored.

```yaml
interval: 10s          # control loop period                        default 10s
listen: ":9090"        # HTTP address                                default :9090
dry_run: false

memory:                # see memory.md; disabled unless dir is set
  dir: /var/lib/dockscale
  snapshot_every: 1m   # save learned state this often              default 1m
  max_state_age: 15m   # older snapshots restore long-term only     default 15m
  evaluate_after: 10m  # grade each decision after this long        default 10m

services:
  - name: web          # required, unique
    min: 2             # >= 1                                        default 1
    max: 10            # >= min                                      default 10

    template:
      image: myapp:1.4         # required; pulled on first use if missing
      cmd: ["serve"]           # optional; overrides the image CMD
      env: ["KEY=value"]
      labels: {}               # e.g. traefik.* routing labels
      network: dockscale_net
      cpus: 0.5                # CPU limit; cpu % is relative to it
      memory: 256m             # memory limit (b, k, m, g)
      stop_timeout: 10s        # SIGTERM → SIGKILL grace           default 10s

    metrics:                   # at least one; the largest recommendation wins
      - type: cpu              # cpu | memory | rps | latency_p95 | queue_depth
        target: 60             # > 0, per replica (see metrics.md)

    prometheus:                # required for rps and latency_p95
      url: http://traefik:8082/metrics
      service: web@docker

    queue:                     # required for queue_depth
      kind: redis_list         # redis_list | rabbitmq
      url: redis://redis:6379/0
      name: jobs
      vhost: "/"               # rabbitmq only                     default /

    behavior:                  # see policy.md
      tolerance: 0.1
      scale_up_cooldown: 30s
      scale_down_cooldown: 2m
      scale_down_stabilization: 5m
      max_step_up: 4
      max_step_down: 1

    model:                     # see models.md
      kind: ewma               # raw | ewma | holt | holt_winters  default ewma
      alpha: 0.5
      beta: 0.3
      gamma: 0.1
      season_length: 24h       # required for holt_winters
      horizon: 3
      outlier_k: 3.5           # -1 disables
      outlier_window: 6

    history:                   # learned load profile, see memory.md
      prewarm: false           # scale ahead of recurring peaks
      cycle: 24h               # or 168h                         default 24h
      slot: 15m                # >= interval, divides cycle      default 15m
      lead: 5m                 #                                 default 5m
      sigma: 1                 # headroom; negative means 0      default 1
      min_samples: 2           #                                 default 2
      decay: 0.25              # (0, 1]                          default 0.25
```

Durations use Go syntax (`500ms`, `30s`, `2m`, `24h`). A field left at zero or
empty takes its default.

## HTTP endpoints

| Endpoint | Content |
|---|---|
| `GET /` | Redirects to the web UI at `/ui/` (see [ui.md](ui.md)) |
| `GET /api/overview`, `/api/series`, `/api/report`, `/api/profile`, `/api/history` | JSON for the UI (see [ui.md](ui.md#api-used-by-the-ui)) |
| `GET /healthz` | `ok`: the process is up |
| `GET /status` | JSON array with one object per service |
| `GET /history?service=&limit=` | Recent decisions with graded outcomes, newest first (default limit 50) |
| `GET /metrics` | Prometheus text format |

`/status` example:

```json
[
  {
    "service": "web",
    "replicas": 4,
    "recommended": 6,
    "desired": 6,
    "signals": [
      {"metric": "cpu", "raw": 47.1, "value": 45.0, "target": 60},
      {"metric": "rps", "raw": 74.2, "value": 70.0, "target": 50}
    ],
    "reason": "cpu=45.0/60.0->3 rps=70.0/50.0->6",
    "last_scale": "2026-10-17T14:02:10Z",
    "updated_at": "2026-10-17T14:02:10Z"
  }
]
```

`/metrics` exposes:

```
dockscale_replicas{service}                 gauge  replicas observed this tick
dockscale_desired_replicas{service}         gauge  policy output
dockscale_signal{service,metric,kind}       gauge  kind = raw | estimate | target
```

Useful alerts: `dockscale_desired_replicas == max` for a long time means the
service is capped and needs a higher `max`. A `last_error` that stays set in
`/status` means scaling is failing.

## Labels

| Label | Set by | Purpose |
|---|---|---|
| `dockscale.managed=true` | dockscale | Marks containers dockscale owns. Nothing without it is ever touched. |
| `dockscale.service=<name>` | dockscale | Groups replicas by service |
| everything in `template.labels` | you | Routing (Traefik) and anything else |

Clean up all replicas, for example after removing a service from the config:

```sh
docker rm -f $(docker ps -aq --filter label=dockscale.managed=true)
```

Removing a service from the config does **not** delete its replicas. They keep
running, unmanaged.

## Logs

Structured `key=value` lines on stderr:

```
level=INFO msg="dockscale started" services=1 interval=10s listen=:9090 dry_run=false
level=INFO msg=scale service=web from=2 to=4 reason="rps=97.3/50.0->4" dry_run=false
level=WARN msg="scrape traefik" service=web err="context deadline exceeded"
level=ERROR msg=scale service=web reached=3 wanted=4 err="create web-1a2b3c: …"
```

## Security

- The memory directory holds decisions, signal values and load profiles.
  It's operational data, not secrets, but it reveals traffic patterns.
- **Access to the Docker socket is root on the host.** Anyone who can control
  dockscale's config, or compromise its process, can start privileged
  containers. Mount the config read-only and limit who can change it.
- dockscale never removes containers without its `managed` label. But anyone
  who can create containers on the host can add that label to one and make
  dockscale count or remove it.
- Queue and Traefik credentials are in the config URLs, so treat the config
  file as a secret.
- The web UI, `/api/*`, `/status` and `/metrics` have no authentication. They're
  read-only, and URLs and env are never published, but they do reveal traffic
  and scaling behaviour. Don't publish port 9090 outside a trusted network.

## Troubleshooting

| Symptom | Check |
|---|---|
| Never scales, reason `no signals` | First tick (CPU and rps need two samples)? Is `prometheus.service` exactly Traefik's service name (`curl traefik:8082/metrics \| grep service=`)? Is the queue reachable? |
| Replicas created but get no traffic | Is `traefik.enable=true` set? Is the network shared with Traefik? Is `--providers.docker.network` correct? |
| CPU always about 0% | Is `template.cpus` much larger than real usage? Or the metric is stuck on first samples. Check `/status`. |
| Scales up and down repeatedly | Raise `tolerance` or `scale_down_stabilization`, or use `ewma`. Confirm the improvement with replay. |
| Slow to react to real spikes | Lower `outlier_window`, raise `alpha`, use `holt` with `horizon` ≈ start-up time ÷ interval |
| `create …: No such image` | Registry login or credentials. Private registries aren't supported yet; pull the image on the host beforehand. |

## Known limitations

- One Docker host only.
- Without `memory.dir`, model state is lost on restart. With it: one dockscale
  per memory directory, and the journal isn't rotated automatically.
- No scale to zero: `min` must be at least 1.
- No credentials for private registries when pulling. Images must already be
  pullable anonymously or pre-pulled.
- Scale-down relies on the app handling SIGTERM gracefully. Traefik stops
  routing to a replica once it stops, but requests already in flight are only
  safe if the app drains them within `stop_timeout`.
