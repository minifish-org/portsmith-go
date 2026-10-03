# Contributing to Portsmith Go

Portsmith Go is a migration workbench backed by the pinned Pith coding-agent
SDK. Contributions should keep execution inspectable, recovery resumable and
acceptance evidence separate from generated code.

## Development setup

Use Go 1.25 or later and Git. Node, npm and Pi are not required for normal Go
development. The first Go dependency download needs network access; the normal
test suite makes no paid model calls.

```sh
git clone https://github.com/minifish-org/portsmith-go.git
cd portsmith-go
go mod download
CGO_ENABLED=0 go build -mod=readonly -trimpath -o bin/portsmith ./cmd/portsmith
./bin/portsmith --help
```

Do not add local `replace` directives to committed `go.mod`. Keep product
dependencies compatible with `CGO_ENABLED=0`.

## Code layout

| Path | Purpose |
| --- | --- |
| `cmd/portsmith/` | CLI process entry point and cancellation |
| `internal/portsmith/` | Analysis, plans, tasks, verification, model backend and workflows |
| `internal/portsmith/sync*.go` | Upstream revision comparison, ownership and incremental planning |
| `internal/portsmith/integration_repair.go` | Durable rollback and final-step repair after project test failures |
| `docs/` | Operator usage, incremental workflow, compatibility and notices |
| `migration/` | Frozen evidence for the original TypeScript-to-Go port |

`docs/source-map.json` records the original source coverage. Native features may
extend the product beyond that port; document them without claiming additional
upstream equivalence.

## Checks before a pull request

```sh
go fmt ./...
CGO_ENABLED=0 go test -mod=readonly -count=1 -timeout=0 ./...
CGO_ENABLED=0 go vet -mod=readonly ./...
CGO_ENABLED=0 go build -mod=readonly -trimpath -o bin/portsmith ./cmd/portsmith
./bin/portsmith --help
git diff --check
```

The tests use temporary directories, Git repositories and deterministic model
stubs. When changing workflow or recovery behavior, add a regression that
demonstrates the failure and the safe recovery path. Do not require a personal
checkout, provider key or live model in the normal suite.

## Changes and review

- Keep code comments, logs and documentation in English.
- Explain the observable behavior changed and how it was checked.
- Preserve frozen-input validation, candidate fingerprints, independent judges
  and whole-project commit gates. A candidate-only pass cannot replace project
  integration acceptance.
- Preserve cancellation recovery and unlimited default execution budgets. Any
  positive budget must be an explicit operator choice.
- Check recorded hashes before replacing or removing transaction-owned files;
  stop and preserve unrelated working-tree changes.
- Keep keys, `.env`, `.cache`, `.portsmith`, binaries and private transcripts out
  of commits. Use placeholder credentials in examples.
- Update operator documentation when command flags or recovery behavior change.

The original `migration/` materials are historical evidence. Do not regenerate
or rewrite accepted contracts, judges, receipts or pins as part of an ordinary
product change. A new upstream port needs its own reviewed frozen plan and fresh
execution records.

## Release packaging

The maintainer packaging command builds macOS and Linux ARM64/AMD64 archives
and a Windows AMD64 archive, with SHA-256 checksums and license/notice texts:

```sh
go run -mod=readonly ./scripts/release.go --version dev --out dist
```

The output directory must be empty. This command downloads the pinned module
graph to collect license texts, but makes no model calls and does not create a
tag or publish a release. Archives contain `BUILD.txt` with the recorded version
and commit. The workflow triggered by an existing version tag creates a draft
release for maintainer review.

## Issues and pull requests

For a bug report, include the command, OS/architecture, Go version, relevant
plan version and redacted error output. For recovery failures, retain your local
journal and candidate so the failure can be inspected; share only the minimal
non-sensitive reproducer.

Keep a pull request focused on one change. Include the checks you ran and any
known limitation. Contributions are made under the repository's
[AGPL-3.0 license](LICENSE); bundled third-party assets retain their own licenses.

For security concerns, follow [SECURITY.md](SECURITY.md) instead of posting keys,
private transcripts or a working exploit in a public issue.
