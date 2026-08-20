# Codex rules

## Required reading

Before changing backend code, read in this order:

1. the root [`AGENTS.md`](../../AGENTS.md);
2. [`../architecture/ARCHITECTURE.md`](../architecture/ARCHITECTURE.md);
3. [`../spec/BACKEND_SPEC.md`](../spec/BACKEND_SPEC.md);
4. relevant accepted records in [`../adr/`](../adr/README.md);
5. [`DEFINITION_OF_DONE.md`](DEFINITION_OF_DONE.md) and any more local
   `AGENTS.md` files.

## Change rules

- Keep changes scoped to the requested behavior.
- Do not change architecture, module boundaries, dependency direction, data
  ownership, or the source-of-truth model without an accepted ADR.
- Do not invent requirements. Record ambiguity and obtain a decision when it
  affects observable behavior or architecture.
- Keep domain logic independent from transport and persistence details.
- Do not add a dependency when the standard library or an existing dependency
  adequately solves the problem; explain every new dependency.
- Add or update tests and documentation alongside behavior changes.
- Never commit secrets, generated credentials, or local environment files.

## Verification

The canonical repository verification command is:

```sh
go test ./...
```

Run it from the repository root. If repository scaffolding or an environment
limitation prevents it from running, report that limitation explicitly; do not
claim successful verification.
