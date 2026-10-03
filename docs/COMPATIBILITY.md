# Portsmith Compatibility

This document states what the Go product preserves from the pinned Portsmith
TypeScript source, what is intentionally adapted, and what is explicitly not
claimed.

## Preserved behavior

- CLI commands, flags, defaults, repeatable `--file`, strict numeric
  validation, JSON schemas and exit statuses (`0`, `1`, and `2` for a version-2
  `needs-preparation` result).
- Version-1 analysis and draft plans, including `analysis.json`,
  `plan.json`, `RULEBOOK.md`, `README.md` and the analysis byte binding.
- Version-1 and version-2 migration workflows, transactional per-unit commits,
  the resumable journal, Git recovery, additive baselines and
  `needs-preparation` preflight.
- Frozen task snapshots: source, rules, `go.mod`, `go.sum`, judge and seed bytes
  are recorded and re-checked by SHA-256. A changed input is rejected rather
  than silently re-read.
- Isolated verification in a scratch module: `gofmt`, compile, `go vet`,
  candidate tests, then the frozen independent judge. Receipts are bound to the
  candidate fingerprint and invalidated when any candidate or manifest byte
  changes.
- The built-in EventStream oracle, the reviewed baseline and the known-broken
  negative control used to prove the judge still detects defects.
- The Pith coding-agent SDK, its persistent `SessionManager`, retry,
  length-continuation, partial-tool rejection and auto-compaction.

## Adaptations

- **Verifier identity.** The Go verifier uses
  `VERIFIER_VERSION = "portsmith-native-go-v1"`. A verification receipt written
  by the TypeScript executor is never treated as current; it is re-verified.
- **Sessions and journals.** The product does not claim byte-compatible
  Pi/Pith session logs. An in-flight TypeScript executor journal is not resumed:
  a new run uses a fresh target/run directory, and completed code plus reviewed
  plans remain portable. Pith session files are written by the pinned SDK and
  are not copied from the TypeScript layout.
- **TypeScript compiler.** TypeScript 5.9.3 is retained as a licensed
  third-party asset embedded at `internal/portsmith/typescript.txt` and executed
  with Goja through small AST and module-resolution bridge snippets. This is not
  a claim that the compiler was ported to Go.
- **Pi JavaScript extensions.** Pi extension `.ts` files are not executed. This
  is reported through progress output. Equivalent behavior must be implemented
  as Go tool definitions or `ToolHooks` passed to `codingagent.NewToolRegistry`
  or the Pith resource loader. The adaptation is documented in
  `docs/THIRD_PARTY.md` and `NOTES.md`.
- **Process control.** Process-group setup and kill are split into
  `process_unix.go` and `process_windows.go` so the binary cross-compiles with
  `CGO_ENABLED=0`.
- **Demo.** `src/demo.ts` is an offline replay helper rather than a bundled
  example tree. See `docs/USAGE.md` for the directory shape and the Go example.
- **Ranges and truncation.** Model context and tool output are not silently
  truncated. Bounded log output is only the explicit `verify` display tail
  (12000 characters) that upstream also printed, and it still labels the full
  report on disk.

## Native incremental additions

The native `sync` command adds Git revision comparison, reviewed TS-to-Go
ownership, reverse import impact analysis, frozen old/new source and v2 drafts.
The optional v2 `updates` manifest authorizes existing-file replacements by
original Git commit and SHA-256, with transactional recovery. Accepted sync
execution advances the upstream baseline through a separate recoverable commit.
These are new Go capabilities, not claims of parity with the original TS CLI.
See [incremental migration](INCREMENTAL.md).

Version-2 whole-project integration failures now reopen the final candidate for
agent repair after a hash-checked, resumable rollback. Earlier checkpoints,
frozen acceptance inputs and the Pith session are preserved. Failure reports are
archived separately from the latest integration result. This repair loop is a
native addition; legacy version-1 integration behavior is unchanged.

## Explicit non-claims

- This product does not claim exhaustive equivalence with the TypeScript
  source, nor that a passing built-in oracle proves full parity.
- A generated file or a model's confidence is not evidence of correctness.
- Legacy plan revision labels are user supplied. Native `sync` requires exact
  Git commit identities and checks exported bytes against Git blobs; frozen
  execution inputs remain checked by SHA-256.
- Local model-controlled shell execution is not an OS security sandbox.
- A standalone Portsmith binary does not bundle arbitrary target compilers.

## Source coverage

`docs/source-map.json` maps every original runtime TypeScript file exactly once
to its actual Go files with a SHA-256 and a `ported`/`adapted` status. Every
adaptation carries a note. The map is a provenance record, not a claim of
exhaustive equivalence.

## Dependency pinning

`go.mod`/`go.sum` pin all Go dependencies; there are no local `replace`
directives and no model-selected dependencies. Runtime Python/Node runtimes are
not required. Go and Git are external development tools required only by
verification and migration integration.

Portsmith's model backend uses Pith's `CreateAgentSession`. The pinned Pith
dependency also provides an optional Durable SDK; adopting that harness in
Portsmith requires a separate integration. The coding-agent's persistent
sessions remain recovery records for agent runs, rather than a general durable
workflow engine.
