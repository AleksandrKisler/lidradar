# 0031: Validate AI output and enforce freshness

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Structured model output may be malformed, inconsistent, low-confidence, duplicated, or based on an obsolete conversation revision.

## Decision

Parse and schema-check versioned output, validate enums, ranges and semantic consistency, enforce confidence policy, then compare conversation revision and analyzed message. Persist runs but mark stale or rejected results without domain mutation.

## Alternatives considered

Trusting provider output and last-write-wins application were rejected because they permit invalid or obsolete facts to change business state.

## Consequences

AI application is deterministic and concurrency-safe; some technically successful runs intentionally produce no domain update.

## Migration and rollback

Validation policies and schemas are versioned. Rollback reprocesses preserved runs only under an explicitly compatible policy and never bypasses freshness checks.
