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

### Recommendations, actions, and outcomes

Every supported Risk type has a deterministic template recommendation, so a
useful corrective instruction does not depend on AI availability. Actions are
tenant-scoped append-only facts attached to a Risk. Outcomes are tenant-scoped
append-only facts attached to an Opportunity; a correction creates another
Outcome rather than rewriting history.

Action and Outcome commands require the `risks.manage` permission and an
`Idempotency-Key`. The key is scoped by tenant and operation: replaying the
same request returns the stored response, while reusing it for different
content returns a conflict. Each successful new Action or Outcome is recorded
in the audit trail atomically with the fact. Cross-tenant identifiers are
indistinguishable from missing resources and cannot produce records.

Architecture changes require an ADR; see [`../adr/README.md`](../adr/README.md).

### Home AI node infrastructure

AI inference uses the outbound pull model from ADR 0030. Registered nodes are
authenticated by a rotatable secret whose SHA-256 digest is the only persisted
form. A ready heartbeat renews only leases currently owned by that node. Jobs
carry the tenant, conversation, base conversation revision, and last analyzed
message identifier.

The default lease is 120 seconds. Claim is atomic, expired work may be reclaimed
after a node disconnect, and a former owner cannot complete a reclaimed job.
Each attempt has a durable AI run with snapshot freshness fields. Successful
inference is recorded independently from application status: invalid output is
`REJECTED`, and a changed revision or analyzed-message identifier is `STALE`.
Neither state mutates domain data. The Go AI agent retains no customer text on
disk, uses outbound calls, and resumes polling after restart. A deterministic
fake provider supports development and disconnect testing without a GPU.

### Revenue and attribution

Revenue confirmation requires `revenue.confirm` and an `Idempotency-Key` and
stores an exact positive decimal amount as a confirmed RevenueEvent. The
RevenueEvent, its single attribution, idempotency response, and audit record
are one atomic write. Key reuse with changed content is a conflict.

Recovered attribution requires a Risk, Action, and Outcome from the same
tenant and Opportunity, each no later than confirmation and within the central
30-day attribution window. Organic and unknown revenue carry no corrective
chain. Confirmed Recovered Revenue sums only confirmed events with a formal
`RECOVERED` attribution and is returned separately per ISO currency; heuristic
association never contributes to this KPI. Cross-tenant references are
indistinguishable from missing resources.

### AI conversation analysis

Conversation analysis uses the versioned `analyze-conversation.v1` contract in
`contracts/ai/analyze_conversation_v1.schema.json`. Context is limited to the
latest 20 messages and a conservative 3,000-token target, and contains the
company context, prior derived summary, task, and output contract. Tenant IDs
are not sent to the model.

Provider output is retained for audit and is strictly decoded before use.
Unknown fields, missing fields, unsupported fact enums, confidence outside
0–1, missing evidence, and inconsistent price facts are rejected. Confidence
of 0.85 or above is strong, 0.65–0.849 is weak, and below 0.65 is untrusted;
untrusted facts are not supplied to domain policies. Model, prompt, schema,
conversation revision, and last-message versions are retained on jobs, runs,
and derived summaries.

Freshness is checked against both the conversation revision and last analyzed
message. A stale run is preserved with `STALE` application status and schedules
a replacement analysis for the current snapshot. Rejected and stale output
cannot mutate Opportunity or Risk state. A fresh valid result may update only
the derived ConversationSummary; later risk features must consume trusted
semantic facts through deterministic policies.

### AI benchmark and model freeze

Conversation-analysis models are compared offline with the versioned JSONL
format `lidradar-ai-benchmark.v1`. Dataset case IDs are unique and cases are
assigned explicitly to `TRAIN`, `VALIDATION`, or `GOLDEN`; a case with no facts
uses an empty `expectedFacts` array rather than omitting its labels. Repository
fixtures contain synthetic content only. The reviewed golden file is protected
by SHA-256 and the runner fails closed when its digest changes.

The runner sends the same versioned request consumed in production, applies the
production output validator and confidence policy, and reports precision,
recall, F1, exact-case rate, invalid output count, p50/p95/p99 latency, and
throughput. Quality and performance thresholds must be supplied explicitly
until product owners approve fixed values. A model manifest may be marked
frozen only after a 300–500 case labelled dataset passes those approved gates on
the target RTX 4060; a missing artifact digest or hardware run leaves it a
candidate.
