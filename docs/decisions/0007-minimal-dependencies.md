# 0007. Minimal dependencies

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

dockscale runs with root-equivalent access (the Docker socket). Every
dependency adds supply-chain risk, upgrade churn and binary size.

## Decision

The only direct dependencies are the Docker SDK (`moby/moby/client`),
`docker/go-units` and `gopkg.in/yaml.v3`. Everything else is written in-repo:

- the Prometheus text parser (fuzz-tested);
- a minimal Redis RESP client (AUTH, SELECT, LLEN);
- the RabbitMQ management API client (plain HTTP and JSON);
- the `/metrics` exposition (hand-formatted);
- persistence (JSONL and JSON files; see 0010).

## Alternatives considered

- **prometheus/common/expfmt:** its parser API changed recently across
  versions (validation-scheme arguments).
- **go-redis:** large for one command.
- **prometheus/client_golang:** heavy for three gauges.
- **SQLite or bbolt for memory:** see 0010.

## Consequences

- A small attack surface and a small build.
- More code of our own to maintain and test. The parser has a fuzz test, which
  has already found one real bug (quantile overshooting its bucket by
  floating-point rounding).
