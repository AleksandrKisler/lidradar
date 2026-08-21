# 0030: Use an outbound pull model for local AI

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

The home AI node must operate behind NAT without exposing model or administration ports to the Internet.

## Decision

Have the AI agent initiate outbound HTTPS heartbeat, claim, status, and result requests. Keep llama.cpp on localhost or an internal container network and retain no customer text on disk.

## Alternatives considered

Inbound public model endpoints, port forwarding, public SSH, and a required VPN endpoint were rejected due to attack surface and deployment friction.

## Consequences

The node needs no inbound reachability and can disappear safely; inference depends on polling and authenticated cloud APIs.

## Migration and rollback

Rotate or replace a node by revoking its secret and letting leases expire. Rollback disables claims without affecting cloud data.
