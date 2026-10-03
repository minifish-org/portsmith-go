# Portsmith Go

A Go port of [Portsmith](https://github.com/minifish-org/portsmith), using [Pith](https://github.com/minifish-org/pith) as its embedded coding-agent SDK.

**Current state: all seven migration steps completed and accepted.** The implementation covers all 16 runtime TypeScript files. Start with [product usage](docs/USAGE.md), [compatibility](docs/COMPATIBILITY.md), and [the accepted migration receipt](migration/results/portsmith.json).

Build the product with `CGO_ENABLED=0 go build -mod=readonly -o bin/portsmith ./cmd/portsmith`, then run `./bin/portsmith --help`. Existing reviewed v1/v2 plans are supported with fresh native execution records. Incremental workflows support frozen read-only baselines and explicitly authorized replacements. The native `sync` command compares upstream Git revisions, follows reverse dependencies, and creates a fresh migration draft. See [incremental sync](docs/INCREMENTAL.md). See [source coverage](docs/source-map.json).

For v2 plans run by the Go product, failed whole-project integration tests also return to the agent repair loop. Portsmith rolls back its uncommitted target files, retains earlier accepted checkpoints and the final candidate/session, and gives the agent the actual failure report. It commits the module only after cumulative acceptance and whole-project tests pass. Archived failures remain available under the final task's `integration-failures/` directory.

## Reproduce the original migration

The following records the original TypeScript-to-Go migration workflow. Its completed execution journal is historical evidence; use a clean destination to reproduce it. It is not the launcher for new migrations performed by the Go product.

Keep the repositories beside one another:

```text
work/
  portsmith/       existing TypeScript executor, with dependencies and dist/ built
  portsmith-go/    this destination repository
  omni-pi/.env    your existing model credentials (not copied into this repository)
```

Use Node 22+, Git, and Go with automatic toolchain downloads enabled. The launcher selects Go 1.25.0 and downloads the pinned module graph before the executor's offline verification. The first preparation may need network access. It defaults to DeepSeek Flash at `https://api.deepseek.com/v1`; credentials come from the supplied environment file or process environment. Existing `OMNI_API_KEY` is supported. No key belongs in a command, plan, source file, or Git commit.

From this directory:

```bash
# Offline with respect to the model: validate every migration step.
node migration/run.mjs --check

# Execute all seven steps using the existing Portsmith + Pi backend.
node migration/run.mjs --commit --env-file ../omni-pi/.env
```

This is one migration command, not seven manual prepare/run/verify commands. It automatically generates, tests, repairs, checkpoints and advances. It first commits the preparation materials, then commits the complete accepted module. It never pushes. `Ctrl-C` preserves the current candidate and agent session; rerun the exact command to resume.

The default model turns, repair attempts and runtime are unlimited. To impose an intentional budget, forward `--max-attempts`, `--max-turns` or `--timeout`; zero disables the corresponding limit. Environment overrides `PORTSMITH_MODEL`, `PORTSMITH_BASE_URL`, `PORTSMITH_MAX_TOKENS` and `PORTSMITH_CONTEXT_WINDOW` remain available. No launcher limit shrinks the model's declared capacities.

Generated candidates appear under `.portsmith/runs/portsmith/<step>/port/candidate/`. Accepted step progress stays in `.portsmith/modules.json`. Final product files are integrated into `internal/portsmith/`, `cmd/portsmith/` and `docs/` after cumulative acceptance. A ready plan is not proof that generated Go code is correct; inspect the final tests, source map, compatibility notes and receipt before release.

## What is being ported

| Step | Behavior |
| --- | --- |
| foundation | Files, locks, child processes, diagnostics, model configuration |
| analysis | TypeScript AST/module resolution, dependency analysis, draft plans |
| workspace | Task snapshots, immutable seeds/judges, editable candidates, hashes |
| verification | Compile/vet/tests, independent judges, live EventStream oracle |
| backend | Pith coding tools, repair loop, durable sessions, retry, compaction |
| workflow | v1/v2 migration, automatic advancement, recovery, additive baselines, commits |
| delivery | Complete CLI, standalone/cross builds, usage and compatibility documentation |

All steps belong to one module because they form one CLI and share the same internal package. Their files remain editable until module acceptance, while earlier independent tests remain cumulative. A step is a recovery checkpoint; a module is the integration/commit boundary.

## Architecture and distribution

```text
Migration now: TypeScript Portsmith -> Pi coding-agent -> DeepSeek Flash
                                     generates and validates Go candidates

Result:        Go CLI -> Go workbench -> Pith coding-agent -> model provider
                           |
                           +-> TypeScript compiler embedded in Goja
                           +-> external Go / Git for verification and commits
```

The product will build with `CGO_ENABLED=0`. No installed Node/npm/Pi is needed by the Go binary. The TypeScript 5.9.3 compiler remains a pinned, licensed JavaScript library embedded in that binary and interpreted by pure-Go Goja; it is not a compiler port. This preserves the original analyzer's AST and TypeScript resolution semantics. Small embedded bridge scripts are allowed; the Portsmith application itself is ported to Go.

Go and Git are still needed for compiling/verifying/integrating generated Go projects. They are operator tools, not a hidden Node runtime. Pi's JavaScript extensions are not automatically portable to Pith; use its Go hooks/resources. Existing TS in-flight journals and Pi session files are not promised to resume in the native executor; start a fresh native run. See [migration rules](migration/RULEBOOK.md).

After migration completes:

```bash
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go build -o bin/portsmith ./cmd/portsmith
./bin/portsmith --help
```

## Review the preparation

```bash
# Compile every frozen judge, verify empty-code rejection, exercise dependency probes.
node migration/audit.mjs

# Check the same v1/v2/resume/additive fixtures against the original executor.
node migration/audit-upstream.mjs
```

The audit makes no model calls. It uses temporary directories and removes them afterward. It proves preparation and a limited set of dependency/negative-control behaviors; it does not prove complete source equivalence. The module's generation task must also port the original regression tests.

`migration/build-plan.mjs` is a maintainer-only material generator. Do not run it during an active migration: changing frozen material invalidates execution receipts. Source is frozen at the revision in [pins.json](migration/pins.json), exported from Git into ignored `.cache/`, and checked by SHA-256. Pith is a published pinned Go dependency, with no local `replace`.

All new product text and documentation are English. Portsmith Go retains the [AGPL-3.0 license](LICENSE); the embedded compiler and Pith keep their own third-party notices.
