# 0029: Separate notifications from delivery attempts

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

A user-visible alert and a retryable transport attempt have different identities and lifecycles.

## Decision

Model Notification as the logical fact and NotificationDelivery as a transport attempt. Deduplicate the logical notification independently from in-app or Telegram retries.

## Alternatives considered

One record per transport call was rejected because retries create duplicate alerts and obscure user intent.

## Consequences

Transport failures can be retried and audited without duplicating alerts, at the cost of two coordinated records.

## Migration and rollback

Transport rollback stops or retries delivery records while retaining logical notifications.
