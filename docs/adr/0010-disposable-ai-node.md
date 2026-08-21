# 0010: Keep AI nodes disposable and non-authoritative

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

The primary AI node is edge hardware and must be replaceable without loss of business data.

## Decision

Store authoritative jobs, runs, and application state in PostgreSQL. AI nodes retain no customer data after processing and can be rebuilt from configuration and model artifacts.

## Alternatives considered

Persistent business storage on the node was rejected because it creates an unmanaged second source of truth.

## Consequences

Node loss affects throughput but not durable data; cloud storage and secure transport carry more responsibility.

## Migration and rollback

Rollback from a node implementation drains leases, replaces the node, and resumes claims from PostgreSQL.
