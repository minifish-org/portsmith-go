# Portsmith Usage

Portsmith is an inspectable, resumable TypeScript-to-Go migration
workbench. The executable is a single Go binary built from `cmd/portsmith`
that embeds the TypeScript compiler and drives the pinned Pith coding-agent SDK
directly. It never shells out to Pi, Node, npm, tsx or a second Portsmith for
production operations.

## Building

```sh
CGO_ENABLED=0 go build -mod=readonly -o portsmith ./cmd/portsmith
```

The binary builds with `CGO_ENABLED=0` for macOS arm64, Linux amd64 and Windows
amd64. Go and Git remain external development tools used to compile, verify and
integrate migrated code; they are not bundled.

## Runtime requirements

| Command | Needs Go? | Needs Git? | Needs a model key? |
| --- | --- | --- | --- |
| `help`, `analyze`, `plan` | no | no | no |
| `prepare` | no | no | no |
| `status`, `next` | no | no | no |
| `judge-check`, `verify` | yes (Go toolchain) | no | no |
| `run`, `migrate --commit` | yes | yes | yes |
| `migrate --check` | no | yes | no |

`analyze` and `help` run with no Node, Go or Git on `PATH`; they are pure Go
plus the embedded TypeScript compiler.

## Commands

Run `portsmith --help` for the complete synopsis. All commands print machine
readable JSON where the source did, and write errors to stderr with exit status
1.

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

The model connection is created lazily on the first generation, reading
`PORTSMITH_*` and then `OMNI_*` variables (optionally loaded from
`--env-file`). Credentials are never printed.

### Incremental migration

A version-2 workflow can freeze existing project files using `baseline` and
record the new migration under a separate `journal`. This supports adding new
capabilities to an existing Go project. The current integration transaction
refuses to overwrite existing product files or receipts, so updating an
already-ported upstream implementation requires additional update-transaction
support. Changing the journal path alone does not enable replacements.

Prepare a fresh reviewed plan for each increment: pinned upstream source,
source-to-Go mappings, output ownership, frozen baseline, contracts and
independent judges. Completed TypeScript executor journals cannot be reused by
the native executor. The tool does not automatically discover upstream releases
or turn a Git diff into an accepted migration plan.

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
