# 0009: Keep AI inference asynchronous

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Local inference can exceed request deadlines and may be unavailable without making core ingestion unavailable.

## Decision

No user API request or webhook waits for LLM inference. Use durable jobs and apply validated results asynchronously.

## Alternatives considered

Synchronous inference was rejected because it couples availability and latency to the model runtime.

## Consequences

Core traffic remains responsive and AI can be disabled; clients observe eventual semantic updates.

## Migration and rollback

A synchronous path requires a superseding ADR. Rollback disables job production or consumption without losing core data.
