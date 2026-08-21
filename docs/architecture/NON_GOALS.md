# MVP architecture non-goals

This document is the guard for backlog item **LR-BE-0005**. It records the
technologies and product directions that are intentionally outside the LidRadar
MVP. They must not be introduced as implementation shortcuts or speculative
infrastructure.

## Prohibited technologies and product directions

The MVP must not add:

- GraphQL;
- WebSocket;
- Redis;
- Kafka;
- NATS;
- Kubernetes;
- Elasticsearch;
- ClickHouse;
- microservices;
- Telegram userbots;
- MTProto production sessions;
- autonomous AI sales;
- automatic customer replies;
- a full CRM;
- a full BI system;
- complex billing;
- a branch-level permission hierarchy;
- a separate mobile application.

These non-goals preserve the approved modular-monolith architecture and keep
PostgreSQL authoritative for durable application state. They also keep the MVP
focused on proving the end-to-end money-recovery workflow rather than building
adjacent products or introducing infrastructure before there is measured need.

## Required alternatives

When designing an MVP change, use the accepted baseline instead of a prohibited
technology:

- expose the public API through REST and its OpenAPI contract, not GraphQL;
- use Server-Sent Events only as an invalidation signal, not WebSocket;
- persist authoritative state, queues, scheduling, idempotency, and outbox
  records in PostgreSQL, not an external cache or message broker;
- keep capabilities as modules and runtime commands in the modular monolith,
  not independently deployed microservices;
- integrate Telegram through the Connected Business Bot connector, not a
  userbot or a production MTProto user session;
- keep AI asynchronous and limited to semantic facts; deterministic domain
  policy owns business state and the system never replies to a customer
  automatically.

## Change control

A feature pull request must not introduce anything in the prohibited list. If a
future requirement cannot be met within these constraints, stop implementation
and propose an ADR that explains the measured need, alternatives, operational
cost, and migration or rollback plan. Implementation may begin only after that
ADR is accepted.

The ADR process is documented in [`../adr/README.md`](../adr/README.md). The
current system boundaries and dependency rules remain authoritative in
[`ARCHITECTURE.md`](ARCHITECTURE.md).
