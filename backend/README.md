# Backend layout

The backend is a modular monolith with five independently built commands:

* `cmd/api`
* `cmd/worker`
* `cmd/scheduler`
* `cmd/ai-agent`
* `cmd/migrate`

Build all five runtime binaries from the repository root:

```sh
make build
```

Every runtime validates typed configuration before starting. Set the required
deployment environment to one of `development`, `test`, `staging`, or
`production`:

```sh
LIDRADAR_ENV=development ./bin/lidradar-api
```

An absent or unsupported `LIDRADAR_ENV` prevents the process workload from
starting and returns a non-zero exit status.

The command writes `lidradar-api`, `lidradar-worker`, `lidradar-scheduler`,
`lidradar-ai-agent`, and `lidradar-migrate` to `bin/`. The four long-running
processes wait for `SIGINT` or `SIGTERM` and then shut down cleanly. The migrate
command currently completes immediately; migration behavior is introduced by
the dedicated foundation task.

Business capabilities live below `internal`. The `risk` package is the
reference module for the canonical layers:

```text
transport -> application -> domain
infrastructure ---------> domain ports
```

Place business rules and persistence interfaces in `domain`, use-case
coordination in `application`, adapter implementations in `infrastructure`,
and protocol-specific handlers and DTOs in `transport`. Shared technical
adapters belong below `platform`; versioned external contracts belong in the
repository-level `contracts` directory.

Run the dependency check from the repository root:

```sh
go run ./backend/tools/archcheck -root backend
```

The check is also mandatory in `.github/workflows/architecture.yml`. Its unit
tests include a negative fixture proving that a `domain` import of `pgx/v5` is
rejected.

Before selecting infrastructure or expanding product scope, check the
documented [MVP architecture non-goals](../docs/architecture/NON_GOALS.md).
Introducing a prohibited technology or product direction requires an accepted
ADR before implementation.
