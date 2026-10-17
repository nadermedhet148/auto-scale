# 0008. moby/moby/client SDK and Go 1.26

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

The first version used `github.com/docker/docker v27.3.1+incompatible`. On the
first real build it failed: `go mod tidy` resolved the newest transitive
dependencies, `go-connections` had removed `sockets.DialPipe`, and the
OpenTelemetry modules required Go ≥ 1.26. `docker/docker` stopped at v28.5.2;
the maintained SDK moved to the `github.com/moby/moby/client` and
`moby/moby/api` modules.

## Decision

- Use **`github.com/moby/moby/client` v0.6.2** (API types from
  `moby/moby/api`), with API version negotiation. It supports Engine API
  versions 1.40–1.56.
- Require **Go 1.26** (`go.mod`, `golang:1.26-alpine` in the Dockerfile).
- Commit `go.sum`. The Dockerfile runs `go mod download` instead of
  `go mod tidy`.

## Alternatives considered

- **Pinning old transitive versions** to keep docker/docker v27: fragile, and
  stuck on an unmaintained SDK.
- **Talking to the Engine REST API directly:** no SDK at all, but we'd have to
  reimplement the Windows named-pipe and TLS transports.

## Consequences

- The modular SDK uses options and result structs
  (`client.ContainerListOptions` → `ContainerListResult.Items`), and errors are
  checked with `containerd/errdefs`.
- Contributors need Go 1.26+.
