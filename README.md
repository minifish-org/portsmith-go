# Portsmith Go

An inspectable, resumable TypeScript-to-Go migration workbench. Portsmith Go
turns a reviewed migration plan into Go candidates, checks them against frozen
acceptance tests, repairs failures with an embedded coding agent, and commits
accepted modules.

It is a Go port of [Portsmith](https://github.com/minifish-org/portsmith), using
[Pith](https://github.com/minifish-org/pith) as its coding-agent backend. The native
executor also supports incremental upstream migration and automatic repair of
whole-project integration failures.

## How it works

```text
Pinned TypeScript source + reviewed plan + independent Go acceptance tests
                                |
                                v
                    Portsmith Go workbench
                      |                 |
                      v                 v
              Pith coding agent     Go / Git
                      |             verification,
                      v             recovery, commits
                Model provider
```

- Analyze TypeScript imports, exports and dependency groups without calling a
  model.
- Freeze source snapshots, contracts, dependency manifests and independent
  judges by hash before execution.
- Generate and repair Go code with Pith's tools, persistent sessions, retry and
  compaction.
- Verify isolated candidates, then run the target project's regression suite.
- For version-2 plans, roll back uncommitted integration files and return test
  failures to the agent for repair. Keep earlier checkpoints and failure reports.
- Resume the same command after interruption. Commit accepted modules; Git push
  remains a separate operator action.
- Compare upstream Git revisions with `sync`, follow reverse import impact and
  prepare a reviewed increment with explicit TypeScript-to-Go ownership.

Portsmith executes the plan; a developer or an external coding agent prepares
and reviews the behavioral contracts, ownership and acceptance tests. A new
upstream hash produces a draft, not an automatically certified port.

## Build and run

Install Go 1.25 or later and Git, then build from source:

```sh
git clone https://github.com/minifish-org/portsmith-go.git
cd portsmith-go
CGO_ENABLED=0 go build -mod=readonly -trimpath -o bin/portsmith ./cmd/portsmith
./bin/portsmith --help
```

The product is a single Go binary and builds without CGO. It embeds its analyzer
and Pith backend; Node, npm and a Pi installation are not required. Go and Git
must still be installed when compiling, verifying and integrating generated Go
projects. See the [command requirements](docs/USAGE.md#runtime-requirements).

## Configure a model

Copy [.env.example](.env.example) to an ignored `.env` file and fill in your
provider credentials locally. This example uses DeepSeek Flash through its
OpenAI-compatible interface:

```dotenv
PORTSMITH_BASE_URL=https://api.deepseek.com/v1
PORTSMITH_MODEL=deepseek-flash
PORTSMITH_API_KEY=replace-with-your-own-key
```

Pass `--env-file .env` to an execution command. `PORTSMITH_*` variables take
precedence over the compatible `OMNI_*` names; existing process variables take
precedence over values loaded from the file. Analysis and preflight checks make
no model calls. Model execution uses your provider account.

## Execute a reviewed migration

For an initial migration, analyze the source and create a draft with `analyze`
and `plan`. Complete the plan, workflow, contracts and independent judges before
running it. See [planning and execution](docs/USAGE.md#planning-and-execution).

Given a prepared plan at `../target/migration/initial`:

```sh
./bin/portsmith migrate --plan ../target/migration/initial --check
./bin/portsmith migrate --plan ../target/migration/initial --commit --env-file .env
```

The execution command advances through all prepared steps and commits accepted
modules. Default model turns, repair attempts and runtime are unlimited. Ctrl-C
preserves progress; repeat the same command to continue. Positive budget flags
are available when you want a limit. See [usage](docs/USAGE.md) and
[integration repair](docs/INCREMENTAL.md#automatic-integration-repair).

## Keep a migrated project current

For an existing Pith checkout, import its migration records and source maps once,
then review and commit the resulting `migration/sync.json`:

```sh
./bin/portsmith sync --init --project ../pith
```

Choose a full upstream Git commit and prepare the increment:

```sh
./bin/portsmith sync --project ../pith --upstream <full-upstream-commit>
```

Review and complete the generated draft's contracts, output ownership and
independent judges. Then execute the whole increment:

```sh
./bin/portsmith sync --project ../pith --upstream <full-upstream-commit> --check
./bin/portsmith sync --project ../pith --upstream <full-upstream-commit> \
  --commit --env-file .env
```

The upstream baseline advances only after complete acceptance. Other migrated
projects can supply the same ownership configuration. See the
[incremental migration guide](docs/INCREMENTAL.md) for the schema, draft files,
review requirements and recovery rules.

## Scope and evidence

This is an experimental migration workbench, not a guarantee of semantic
equivalence. Passing tests prove the checked behaviors. Review the generated
code and ensure the acceptance contracts cover the behavior you depend on.

The analyzer embeds the licensed TypeScript 5.9.3 JavaScript compiler and runs
it in pure-Go Goja. The compiler itself has not been rewritten in Go. Pi
JavaScript extensions are not executed; equivalent behavior needs Go tools or
Pith hooks. Local model-controlled shell execution is not an OS security
sandbox. Run migrations in a dedicated working copy with appropriate local
permissions.

The original migration ported all 16 runtime TypeScript files through seven
accepted steps. Its [receipt](migration/results/portsmith.json),
[source map](docs/source-map.json) and [compatibility notes](docs/COMPATIBILITY.md)
record that scope and its adaptations. Native additions are documented
separately from that historical acceptance.

## Contribute and reproduce

Start with [CONTRIBUTING.md](CONTRIBUTING.md) for the code layout and offline
checks. Product code, logs and documentation are English.

The scripts under `migration/` record the original TypeScript Portsmith + Pi
migration of this repository. They require the original TypeScript executor and
Node 22+, and are not needed by the Go product. Reproduction must use a clean
destination and the pinned material in [migration/pins.json](migration/pins.json).
Do not regenerate frozen migration inputs during an active run.

The historical offline checks are `node migration/run.mjs --check`,
`node migration/audit.mjs` and `node migration/audit-upstream.mjs`. The historical
execution launcher is `node migration/run.mjs --commit --env-file <local-env>`;
it calls a model. See [migration rules](migration/RULEBOOK.md) before reproducing.

## License

Portsmith Go is licensed under [AGPL-3.0](LICENSE). Embedded assets and
dependencies retain their own licenses; see [third-party notices](docs/THIRD_PARTY.md).
