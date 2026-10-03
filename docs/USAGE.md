# Portsmith Usage

Portsmith is an inspectable, resumable TypeScript-to-Go migration
workbench. The executable is a single Go binary built from `cmd/portsmith`
that embeds the TypeScript compiler and drives the pinned Pith coding-agent SDK
directly. It never shells out to Pi, Node, npm, tsx or a second Portsmith for
production operations.

## Building

```sh
CGO_ENABLED=0 go build -mod=readonly -trimpath -o bin/portsmith ./cmd/portsmith
./bin/portsmith --help
```

The binary builds with `CGO_ENABLED=0` for macOS arm64, Linux amd64 and Windows
amd64. Go and Git remain external development tools used to compile, verify and
integrate migrated code; they are not bundled.

The commands below use `portsmith` as shorthand for `./bin/portsmith`, or for a
binary you have placed on `PATH`. Build from the repository root.

## Runtime requirements

| Command | Needs Go? | Needs Git? | Needs a model key? |
| --- | --- | --- | --- |
| `help`, `analyze`, `plan` | no | no | no |
| `prepare` | no | no | no |
| `status`, `next` | no | no | no |
| `judge-check`, `verify` | yes (Go toolchain) | no | no |
| `run`, `migrate --commit` | yes | yes | yes |
| `migrate --check` | no | yes | no |
| `sync --init`, `sync`, `sync --check` | no | yes | no |
| `sync --commit` | yes | yes | yes |

`analyze` and `help` run with no Node, Go or Git on `PATH`; they are pure Go
plus the embedded TypeScript compiler.

## Commands

Run `portsmith --help` for the complete synopsis. Result objects are printed as
JSON; execution commands also print progress. Errors go to stderr with exit
status 1. See [exit status](#exit-status) for the preparation result.

## Planning and execution

Portsmith separates planning from implementation. `analyze` and `plan` create
factual analysis and an initial draft without calling a model. A developer or
external coding agent must turn the draft into a reviewed executable plan.

For a first migration, use a pinned source checkout and a separate target Git
repository. The target needs a committed `go.mod` and, when dependencies require
it, `go.sum`. Choose and test the Go dependencies before freezing the workflow.

```sh
portsmith analyze --source ../upstream --out ../target/migration/analysis.json
portsmith plan --analysis ../target/migration/analysis.json \
  --out ../target/migration/initial --revision <pinned-source-revision>
```

Give the draft to the reviewer with this task:

> Complete a version-2 migration plan for this source and target. Define the
> module boundaries, ordered steps, behavioral contracts, output ownership,
> frozen independent Go judges and workflow configuration. Cover observable
> behavior and dependency adaptations. Mark a batch ready only when its
> contracts and acceptance tests are complete. Preserve the frozen source and
> dependency manifests; do not treat generated candidate tests as independent
> evidence.

The reviewer must understand the source behavior; a generated directory-based
draft is not a complete specification. For the v2 schema, the reviewed
[`migration/plan.json`](../migration/plan.json) and
[`migration/workflow.json`](../migration/workflow.json) are historical examples.
Their recorded execution applies to this repository, so do not reuse their
journal or receipts for a new target.

Then preflight and execute the entire prepared plan:

```sh
portsmith migrate --plan ../target/migration/initial --check
portsmith migrate --plan ../target/migration/initial --commit --env-file .env
```

No per-file `prepare`, `run`, `verify` or `accept` loop is required for a reviewed
workflow. These lower-level commands remain useful for inspecting a single
task. Version-1 plans are also supported, with their original workflow behavior.
For an already migrated project, prefer the [incremental guide](INCREMENTAL.md).

## Model configuration

Store credentials in an ignored local `.env`, or supply them through your
process environment:

```dotenv
PORTSMITH_BASE_URL=https://api.deepseek.com/v1
PORTSMITH_MODEL=deepseek-flash
PORTSMITH_API_KEY=replace-with-your-own-key
```

The backend uses an OpenAI-compatible Chat Completions endpoint with tool
calling. `PORTSMITH_BASE_URL`, `PORTSMITH_MODEL` and `PORTSMITH_API_KEY` take
precedence over `OMNI_BASE_URL`, `OMNI_MODEL` and `OMNI_API_KEY`. Loading
`--env-file` never replaces an existing process variable. If no file is
specified, an existing `.env` in the working directory is loaded lazily when
execution first needs a model.

Known model capacities come from the pinned Pith catalog. Optional
`PORTSMITH_CONTEXT_WINDOW` and `PORTSMITH_MAX_TOKENS` overrides are validated
against those capacities. The model's input/output usage is reported when the
provider supplies it; compatible providers may not report a monetary cost.
Never commit real credentials, session logs or unreviewed provider transcripts.

## Command reference

### analyze

```sh
portsmith analyze --source <source> --out <analysis.json>
```

Walks the source tree, hashes every TypeScript-family file and every
`package.json`/`tsconfig*.json`, resolves imports and reports strongly connected
components. This step never calls a model. The JSON schema is version 1 and
matches `src/analyze.ts`.

### plan

```sh
portsmith plan --analysis <analysis.json> --out <plan-directory> --revision <revision>
```

Writes `analysis.json`, `plan.json`, `RULEBOOK.md` and `README.md` into a new
plan directory. The plan is a draft grouped by source directory; review and
complete the goal, `targetPackage`, acceptance scenarios and dependencies
before execution.

### prepare

```sh
# From a reviewed plan unit:
portsmith prepare --plan <plan-directory> --unit <unit-id> --out <task-directory>

# From an explicit file list:
portsmith prepare --source <source> --out <task-directory> --revision <revision> \
  --file <relative-file> [--file <test>] --goal <goal>

# Built-in event-stream example:
portsmith prepare --source <pi-source> --out <task-directory> \
  --revision <revision> --example event-stream
```

`--file` is repeatable. Optional frozen inputs are `--rules <rules.md>`,
`--go-mod <go.mod>`, `--go-sum <go.sum>` and `--judge <judge-directory>`. The
destination is created fresh; an existing task directory is never overwritten.

### run

```sh
portsmith run --task <task-directory> [--env-file <file>] \
  [--feedback <review.md>] [--max-turns <turns>] [--timeout <seconds>]
```

Runs one durable Pith session against the prepared task. Native Pith tools plus
the custom `verify_candidate` tool are active. `0` means unlimited for
`--max-turns`, `--timeout` and `--max-attempts`. Ctrl-C cancels through the
process context and preserves progress.

### judge-check

```sh
portsmith judge-check --task <task-directory>
```

Validates the built-in EventStream judge: it runs the live source oracle,
confirms it still matches the reviewed baseline, and confirms the injected
judge catches a known-broken implementation.

### verify

```sh
portsmith verify --task <task-directory> [--allow-download]
```

Runs the isolated Go verifier: `gofmt`, compile, `go vet`, the candidate's own
tests and then the frozen independent judge in a scratch copy. `--allow-download`
permits verifier dependency downloads. A receipt is written to
`verification.json` and is bound to the candidate fingerprint.

### status / next

```sh
portsmith status --task <task-directory>
portsmith status --plan <plan-directory> --runs <runs-directory>
portsmith next   --plan <plan-directory> --runs <runs-directory>
```

`status --task` reports `prepared`, `generated`, `stale_verification`, a
verification status, `accepted` or `export_changed`. `status --plan` reports one
entry per planned unit. `next` keeps only unblocked, unfinished units.

### accept

```sh
portsmith accept --task <task-directory> --out <new-export-directory>
```

Exports a freshly verified candidate. Acceptance requires a current,
independent `behavior_verified` receipt; compilation or candidate tests alone
are not enough.

### migrate

```sh
portsmith migrate --plan <plan-directory> --check
portsmith migrate --plan <plan-directory> --commit \
  [--env-file <file>] [--max-attempts 0] [--max-units 1] [--allow-download]
```

`--check` is an offline preflight: it validates the prepared workflow and makes
no model calls, so it needs no key. `--commit` performs transactional per-unit
integration and commits; it never pushes. Version-1 plans and version-2
module plans are both supported. Progress is resumable from the journal.

With version-2 plans, failed whole-project tests automatically reopen the final
candidate for agent repair. Earlier accepted checkpoints and the agent session
survive; uncommitted transaction files are restored before generation resumes.
Full failure reports remain under the final task's `integration-failures/`.
The same command continues through cumulative acceptance, integration and the
module commit. See [integration repair and recovery](INCREMENTAL.md#automatic-integration-repair).

The model connection is created lazily on the first generation, reading
`PORTSMITH_*` and then `OMNI_*` variables (optionally loaded from
`--env-file`). Credentials are never printed.

### Incremental migration

```sh
# Import the existing Pith migration and source maps once; review and commit it.
portsmith sync --init --project ../pith

# A full upstream commit creates a deterministic draft, without model calls.
portsmith sync --project ../pith --upstream <full-new-Pi-hash>

# After contracts, ownership and independent judges have been reviewed:
portsmith sync --project ../pith --upstream <full-new-Pi-hash> --check
portsmith sync --project ../pith --upstream <full-new-Pi-hash> --commit \
  --env-file .env
```

The execution command runs all reviewed steps with Pith, verifies and repairs
candidates, commits accepted modules and advances the sync baseline only after
complete acceptance. Rerun it to resume. No manual task-by-task loop is needed.
It never pushes. See [the complete incremental guide](INCREMENTAL.md).

### help

```sh
portsmith --help
portsmith help
```

## Offline demo workflow

The TypeScript `src/demo.ts` replayed the bundled `examples/event-stream` tree
without any model calls. The Go repository does not bundle that example tree, so
the equivalent workflow is the `DemoTask` helper in
`internal/portsmith/demo.go`. It accepts any directory with the same shape:

```
<example>/
  source/
    LICENSE
    packages/ai/src/utils/event-stream.ts
    packages/ai/test/event-stream.test.ts
  reference-go/
    event_stream.go
    event_stream_test.go
    NOTES.md
```

A small program can drive the same offline replay:

Place this helper inside this repository, for example under
`cmd/offline-demo/main.go`; Go's `internal` package rule prevents importing it
from an unrelated module. The helper still needs the example files above.

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/minifish-org/portsmith-go/internal/portsmith"
)

func main() {
	root, report, err := portsmith.DemoTask(context.Background(),
		portsmith.DemoOptions{Example: "examples/event-stream"},
		func(message string) { fmt.Println(message) })
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(root, report.Status)
	if report.Status != "behavior_verified" {
		os.Exit(1)
	}
}
```

## Exit status

- `0` success.
- `1` error, unknown command, failed verification or a run that did not reach
  `candidate_ready`.
- `2` a version-2 migration reported `needs-preparation` (gaps must be prepared
  before model calls).

## Budgets and cancellation

Zero means unlimited, exactly as upstream. Positive `--max-turns`,
`--max-attempts`, `--max-units` and `--timeout` values are honored.
`--max-turns` and `--timeout` are validated as non-negative safe integers and
`--timeout` is bounded to 2147483 seconds. `--max-units`, when present, must be
positive.
