# 0002. Single host, Docker Engine API, labels as the source of truth

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

The goal was autoscaling for plain Docker, as "a native system on top of
Docker". The options were a single host, Docker Swarm, or a custom
multi-host agent system.

## Decision

- Target **one Docker host** first, managed through the **Docker Engine API**
  (the socket).
- A replica is any container labelled `dockscale.managed=true` and
  `dockscale.service=<name>`. Docker's list of containers with those labels is
  the **only** source of truth for the replica count, and it's re-read every
  tick.
- dockscale never touches containers without its labels.

## Alternatives considered

- **Docker Swarm (`docker service scale`):** simpler scaling, but it requires
  Swarm mode and gives up control of placement and naming.
- **A multi-host agent and controller:** far more work. It remains on the
  roadmap, behind the same engine.
- **dockscale tracking its own replica count:** it would drift from reality
  after crashes and manual changes.

## Consequences

- Restarts are safe: existing replicas are adopted by label.
- Containers removed by hand are replaced on the next tick.
- Anyone who can create containers can fake the labels (see the security
  section in operations.md).
- No multi-host scheduling.
