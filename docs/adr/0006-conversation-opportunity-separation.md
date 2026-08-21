# 0006: Separate conversations from opportunities

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

A long-lived customer dialogue can contain distinct commercial attempts with different lifecycles.

## Decision

Model Conversation and Opportunity as separate aggregates. During MVP, enforce at most one active Opportunity per Conversation.

## Alternatives considered

A single combined aggregate was rejected because communication history and commercial state have different identities and lifetimes.

## Consequences

Commercial logic stays focused and future opportunities can reuse conversation history; workflows must coordinate two aggregates.

## Migration and rollback

Schema consolidation requires a superseding ADR and lossless migration. Rollback reconstructs opportunity references from preserved identifiers.
