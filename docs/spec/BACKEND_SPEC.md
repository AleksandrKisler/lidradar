# Backend specification

## Purpose

The LidRadar backend is implemented in Go as a modular monolith. This document
is the starting point for backend requirements and contracts. Feature-specific
behavior should be added here, or linked from here, before it is implemented.

## Baseline contract

- PostgreSQL is the source of truth for persistent application state.
- Business capabilities must respect the module and dependency rules in
  [`../architecture/ARCHITECTURE.md`](../architecture/ARCHITECTURE.md).
- Inputs are validated at the system boundary; domain invariants are enforced
  inside the owning module.
- Failures must be represented deliberately and must not expose credentials,
  internal implementation details, or sensitive data.
- State changes that must succeed or fail together use an explicit PostgreSQL
  transaction with a clearly identified owner.
- External or asynchronous side effects must account for retries and
  idempotency; they must not override PostgreSQL as the authoritative state.

## Feature specifications

### NO_RESPONSE risk

The Risk module owns the `Risk` aggregate and active-risk deduplication. A
`NO_RESPONSE` check uses a versioned deterministic policy; it does not invoke
AI. The policy creates or refreshes a risk only when all of these conditions
hold at execution time:

- the last meaningful canonical message is incoming;
- no outgoing message exists after that triggering message;
- the related Opportunity is active; and
- at least the Location response threshold has elapsed in the Location's IANA
  timezone and weekly business-hours schedule.

The first 45–89 elapsed business minutes have `HIGH` severity and 90 or more
have `CRITICAL` severity. Closed periods do not contribute to elapsed time. A
threshold crossing outside the current working period is carried into the next
working period.

Scheduled work contains tenant and Opportunity identifiers plus its due time,
not authoritative conversation state. The worker must reload current canonical
state before evaluation. A reply or an inactive Opportunity prevents creation
and resolves any active `NO_RESPONSE` risk. Replayed or concurrent checks
atomically create at most one active risk per tenant, Opportunity, and risk
type; later positive evaluations refresh its evidence instead.

All repository operations require both tenant and Opportunity identifiers.
PostgreSQL is the production source of truth and must enforce active-risk
uniqueness. Cancellation and persistence errors are returned to the worker for
its normal retry classification; invalid or cross-tenant state is rejected and
must not mutate a risk.

Feature-level backend contracts added later must likewise define observable
behavior, data ownership, error behavior, and operational requirements before
production code is added.

### Radar and Risk realtime API

Radar reads and commands are tenant-scoped and authorized through the named
`risks.read` and `risks.manage` membership permissions. The list is ordered by
active state, severity (`CRITICAL` before `HIGH`), oldest detection time, and a
stable ID tie-breaker, and uses cursor pagination. Risk detail includes related
Opportunity, Conversation, Recommendation, Action, Outcome, and Revenue data
only when those owning modules have produced it. Exact money totals are exposed
as decimal strings.

Acknowledge and resolve commands are idempotent. Cross-tenant identifiers are
indistinguishable from missing resources and cannot mutate state. SSE carries
only tenant-scoped invalidation signals after durable changes; clients always
refetch the authoritative REST read model, and losing an SSE signal never loses
business data. The versioned HTTP contract is maintained in
[`../../contracts/openapi/openapi.yaml`](../../contracts/openapi/openapi.yaml).

### Telegram risk notifications

The Notification module owns the logical Notification, transport delivery
attempts, one-time Telegram link tokens, and installed Telegram user links. A
risk-opened alert uses the deterministic key `risk:{risk_id}:opened`; replaying
the event or retrying Telegram cannot create another user-visible notification.
Notification intent and its initial delivery are persisted atomically before
the external request. Each retry is retained as a separate delivery attempt,
using the standard retry schedule, and Telegram failure never changes Risk
state.

Link token plaintext is never persisted: only its SHA-256 digest, expiry, and
single-use timestamp are stored. Telegram callbacks require a tenant-scoped
user link and idempotency key and are restricted to `OPEN_RISK`, `ACKNOWLEDGE`,
and `SNOOZE`; financial mutations are not accepted through this boundary.

Architecture changes require an ADR; see [`../adr/README.md`](../adr/README.md).
