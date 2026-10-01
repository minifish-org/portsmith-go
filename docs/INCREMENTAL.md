# Incremental upstream migration

Portsmith Go is the primary executor. TypeScript Portsmith remains the original
reference; these new capabilities are native Go features. The model backend is
Pith's coding-agent SDK, with the same DeepSeek configuration as ordinary
`migrate` runs. No Node/npm/Pi installation is needed.

## One-time source ownership

From the Portsmith Go checkout, build the executable and initialize a migrated
Pith project:

```sh
CGO_ENABLED=0 go build -mod=readonly -o bin/portsmith ./cmd/portsmith
./bin/portsmith sync --init --project ../pith
```

Initialization imports `migration/upstream.json`, the original and SDK module
plans and `packages/**/source_map*.json`. Fine source maps take precedence;
otherwise an implementation source maps conservatively to its batch's Go outputs.
Reference-only inputs without fine ownership are recorded separately in
`references`: their changes require review but never unlock unrelated Go files. Review
`../pith/migration/sync.json` and commit that file before preparing an increment.
Do not run initialization twice. Other migrated projects can provide the same
version-1 config directly:

```json
{
  "version": 1,
  "repository": "https://github.com/owner/upstream",
  "revision": "0123456789abcdef0123456789abcdef01234567",
  "roots": ["packages/ai/src", "packages/agent/src"],
  "references": ["packages/ai/test/provider.test.ts"],
  "mappings": [
    {"source": "packages/ai/src/types.ts", "goFiles": ["packages/ai/types/types.go"]}
  ]
}
```

The revision is the last fully accepted upstream commit. Commit source ownership
changes before preparing a plan; a changed config cannot reuse an existing
plan with another mapping. `--config` selects another file under `migration/`.

## Prepare a real increment

Choose a full 40-character upstream Git commit, then run:

```sh
./bin/portsmith sync --project ../pith --upstream <new-Pi-hash>
```

Without `--source`, the tool clones/fetches an ignored bare upstream mirror.
To use an existing local Git checkout instead:

```sh
./bin/portsmith sync --project ../pith --source ../pi \
  --upstream <new-Pi-hash>
```

Both old and new commit objects must exist. There are no model calls at this
stage. The command compares added/modified/deleted files within the reviewed
roots, hashes their actual Git blobs, records unmapped changes, and follows
reverse TS import dependencies using both old and new source trees. A JSON
configuration change conservatively includes all mapped source. Upstream
snapshots are exported directly from Git blobs and checked before analysis.
Archive attributes cannot rewrite their bytes or omit tracked files. Older
archive caches are repaired only when their bytes exactly match Git's archive
representation; other content changes still fail validation.

The default draft is `../pith/migration/sync/pi-<12-character-new-hash>/`:

- `sync-report.json`: revisions, exact changes, mappings, unmapped inputs and
  affected sources.
- `analysis.json`: native TS analysis, including frozen old source inputs.
- `plan.json`: affected source and Go output ownership, grouped into one module.
- `new-mappings.json`: explicit mappings to add/update after acceptance; empty
  until review assigns actual ownership. Excluded source is not silently mapped.
- `workflow.json`: read-only baseline, existing writable updates and a separate
  journal, initially `planned`.
- `RULEBOOK.md`, `README.md`: preparation and implementation requirements.

Old and new upstream bytes live in ignored `.cache/portsmith-upstream/`.
`sync-before/<old-hash>/<source>.txt` preserves old versions including deleted
files. Fresh native journals live under `.portsmith/sync/`; historical migration
journals and receipts stay untouched. `--out migration/<name>` chooses another
new plan directory. Repeating the same command preserves reviewed materials.
Exit `2` means the draft needs preparation, not that generation failed.

## Review once, execute the whole plan

Give Codex the draft and ask it to complete the migration plan, using the
existing v2 workflow schema. This is the planning/review stage we already use,
not a manual execution command for every file.

For every changed behavior, define the contract and frozen independent Go
judges. Resolve unmapped/new/deleted sources: assign output ownership or record
why they are intentionally outside the embedded SDK. Add candidate self-tests
to `plan.batches[].outputs`, then fill `workflow.batches.changes.steps` with
`sources`, `goal`, `contract`, `judge`, `outputs` and independent `tests`, and
mark the batch `ready`. Put new or corrected TS-to-Go ownership in
`new-mappings.json` using the same mapping entries as the config; targets must be
reviewed Go outputs. Leave excluded sources out of this file. Sources referenced by steps must be in the batch and
frozen analysis. If new writable targets were previously baseline files, move
their original hashes to `updates.files`. Neither old acceptance tests nor the
new judge should be changed by the implementation agent.

The tool supplies factual differences; it does not invent accepted semantics
or certify that the reviewer covered every upstream change. A new hash alone
is insufficient for reliable autonomous behavioral porting. Renames appear as
additions/deletions; automatic Go file deletion, dependency-manifest changes
and symbol rename decisions are intentionally not inferred.

Then run from the Portsmith Go checkout:

```sh
./bin/portsmith sync --project ../pith --upstream <new-Pi-hash> --check
./bin/portsmith sync --project ../pith --upstream <new-Pi-hash> --commit \
  --env-file ../omni-pi/.env
```

Use the same `--source`, `--config` and `--out` if you supplied them originally.
`--check` makes no model calls. `--commit` runs the reviewed plan through Pith,
repairs until acceptance, commits modules, then commits the advanced upstream
revision and newly included source ownership. A partial or failed run keeps the
old baseline. Zero budgets mean unlimited. Ctrl-C preserves progress; rerun
exactly the same execution command. Git push is always a separate operator step.

`migrate --plan <draft> --commit` also works, but does not advance sync metadata;
run `sync ... --commit` afterward to finish that step without regenerating code.
If the new revision has no scoped changes, `sync` reports `no_changes` and retains
the previous baseline; this keeps ignored changes out of acceptance history.

## Existing-file safety and recovery

A v2 workflow's optional `updates` uses the same full target commit and named
SHA-256 format as `baseline`. Its files must be regular tracked files under
`packages/`, `internal/`, `cmd/`, `docs/` or `examples/`, explicitly owned by
planned outputs. Existing independent judges, manifests and historical receipts
remain outside this writable set. A separate native journal is mandatory.

Original writable files enter the candidate as editable initial code. Other
updates and baseline files enter as read-only dependencies. Accepted replacement
bytes and original hashes are recorded before integration, then files are
replaced atomically. Project regression tests still run before the commit.
Unchanged output bytes are not included as fake changed paths.

Recovery accepts only the recorded old bytes or verified new bytes. A third
hash is an external edit and is preserved with an error. Recovery also recognizes
an already completed exact commit, without regenerating or committing twice.
The final metadata commit has its own durable transaction, so interruption or
failed Git hooks do not silently lose the baseline update. Resolve unrelated
working-tree changes before resuming; do not delete journals to bypass checks.

## Validation scope

Tests use real temporary Git repositories and a deterministic generation stub;
they exercise differences, import propagation, deleted source preservation,
planning gates, cache tampering, accepted replacements, drift refusal,
integration recovery and metadata-commit recovery. They do not spend provider
credits or claim complete equivalence of arbitrary upstream behavior.
