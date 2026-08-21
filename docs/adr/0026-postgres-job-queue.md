# 0026: Use a leased PostgreSQL job queue

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Background work must survive worker crashes and coordinate retries without adding another authoritative system.

## Decision

Store jobs in PostgreSQL, claim with `FOR UPDATE SKIP LOCKED`, use expiring leases, classify retryable and permanent failures, and finish irrecoverable jobs as DEAD.

## Alternatives considered

In-memory workers were rejected because crashes lose work. An external broker was rejected because PostgreSQL already owns durable state and MVP scale does not justify it.

## Consequences

Jobs are crash recoverable and transactionally composable, with database load that must be monitored.

## Migration and rollback

A future broker migration must preserve job identity and leases. Rollback stops claims and resumes the PostgreSQL queue.
