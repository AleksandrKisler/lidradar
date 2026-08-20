# Architecture decision records

Architecture decision records (ADRs) explain significant changes to LidRadar's
architecture. An ADR is required before changing the modular-monolith shape,
module boundaries, dependency direction, data ownership, PostgreSQL
source-of-truth policy, or cross-module communication model.

## Workflow

1. Create `NNNN-short-title.md` in this directory using the next available
   four-digit number.
2. Describe the context, decision, alternatives, consequences, and migration or
   rollback considerations.
3. Mark the record `Proposed` while it is under discussion.
4. Obtain project approval and mark it `Accepted` before implementation.
5. Supersede old decisions with a new ADR rather than rewriting their history.

## Minimal template

```md
# NNNN: Decision title

- Status: Proposed
- Date: YYYY-MM-DD

## Context

## Decision

## Alternatives considered

## Consequences

## Migration and rollback
```

This index contains no accepted architecture decisions yet. The current
architecture guardrails are documented in
[`../architecture/ARCHITECTURE.md`](../architecture/ARCHITECTURE.md).
