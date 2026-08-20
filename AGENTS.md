# LidRadar repository map

LidRadar is a **modular monolith** with a **Go backend**. PostgreSQL is the
system of record (source of truth).

Before changing the backend, read:

1. [`docs/architecture/ARCHITECTURE.md`](docs/architecture/ARCHITECTURE.md) — system boundaries and dependency rules.
2. [`docs/spec/BACKEND_SPEC.md`](docs/spec/BACKEND_SPEC.md) — backend contract and constraints.
3. [`docs/engineering/CODEX_RULES.md`](docs/engineering/CODEX_RULES.md) — rules for automated changes.
4. [`docs/engineering/DEFINITION_OF_DONE.md`](docs/engineering/DEFINITION_OF_DONE.md) — completion checklist.

Architecture decisions live in [`docs/adr/`](docs/adr/README.md). **Do not make
architectural changes without an accepted ADR.**

Canonical verification command: `go test ./...`.
