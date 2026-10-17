# 0001. Record architecture decisions

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

dockscale was designed and built in a few intense iterations. Many choices
(why Traefik, why no ML, why plain files) were made in conversation and
weren't visible in the code. Future contributors, including AI coding
sessions that start with no context, need the *why*, not just the *what*.

## Decision

Keep architecture decision records in `docs/decisions/`, one Markdown file per
decision, numbered, using `template.md`. Record decisions as they're made.
Keep `CHANGELOG.md` for what changed and `CLAUDE.md` for how to work in the
repo.

## Alternatives considered

- **Commit messages only:** hard to find, and they describe changes, not
  reasoning.
- **A single design doc:** grows stale, and its history of reasoning is lost
  when it's rewritten.

## Consequences

A small overhead per decision. Accepted records are not rewritten; changing a
decision means writing a new record that supersedes the old one.
