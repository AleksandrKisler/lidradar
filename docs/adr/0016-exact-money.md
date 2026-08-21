# 0016: Represent money with exact decimals

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Revenue calculations cannot tolerate binary floating-point rounding or ambiguous JSON numbers.

## Decision

Use PostgreSQL `NUMERIC(14,2)`, decimal values in Go, and JSON strings containing amount plus currency. Never use float32 or float64 for money.

## Alternatives considered

Binary floating point and minor-unit integers without a fixed currency exponent were rejected for contract clarity and correctness.

## Consequences

Arithmetic and serialization are exact, with explicit parsing and validation overhead.

## Migration and rollback

Changing representation requires a lossless migration and contract version. Rollback parses decimal strings into the existing numeric columns.
