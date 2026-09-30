# Embedded SDK compatibility

This table freezes the compatibility scope of the Go embedded SDK
(`packages/coding-agent`) relative to Pi `f07218c4d4bbc12bef056a7058c3dd49dfe41abe`.
It is deliberately conservative: a checked-in test proves the covered contract
only. Passing the cumulative and independent tests is **not** proof of full Pi
behavior or production readiness.

Status legend:

- **Ported** — Go implementation with matching public behavior.
- **Adapted** — Go shape differs for an idiomatic or headless reason; the
  difference is recorded in the reason column of the source map.
- **Excluded** — intentionally outside this increment, with a concrete reason.

## Core session and provider surface

| Pi area | Status | Go surface | Notes |
| --- | --- | --- | --- |
| `createAgentSession` | Ported | `CreateAgentSession`, `SessionOptions` | Returns `*AgentSession` plus error; services are built internally. |
| Session events | Adapted | `SessionEvent` + `SessionEventType` | One discriminated union replaces the TS event hierarchy; callers switch on `Type`. |
| Streaming / partial updates | Adapted | `Subscribe` callbacks, `RunResult` | Headless callers observe events and the final result; no TUI rendering. |
| Message/transcript conversion | Ported | `ConvertToLlm` | Reuses the accepted harness message conversion. |
| Model resolution | Ported | `ResolveModel`, `ResolveSessionModel` | Explicit overrides only; capacity is never reduced. |
| Model/provider runtime | Ported | `ModelRuntime`, `ModelRegistry` | Reuses the Pith AI catalog, auth and provider composition. |
| Provider protocols | Ported (reused) | `packages/ai/api`, `packages/ai/providers` | The SDK does not reimplement provider wire protocols. |
| Credential callbacks | Ported | `ModelOptions.APIKey` | Per-request resolution; no global credential cache. |
| Headless provider selection | Ported | native `Api` dispatch | Model API selects the provider when `StreamFn` is nil. |
| Usage reporting | Ported | `RunResult.Usage`, `SessionStats`, `UsageTotals` | Aggregated per run and session. |

## Sessions, storage and compaction

| Pi area | Status | Go surface | Notes |
| --- | --- | --- | --- |
| Durable session file | Ported | `OpenSession`, `SessionManager` | Append-only JSONL tree; version 3. |
| Branching / tree navigation | Ported | `Branch`, `BranchWithSummary`, `GetTree`, `GetBranch` | Leaf changes are durable. |
| Session projection/context | Ported | `BuildSessionProjection`, `BuildSessionContext` | Context edits and compaction entries are applied. |
| Auto-compaction | Ported | `RunPolicy.CompactReserveTokens`, `Compact` | Caller supplies `Summarize`. |
| Branch summarization | Ported | `PrepareBranchEntries`, `GenerateBranchSummary` | Reuses the accepted harness compaction package. |
| Retry of transient provider errors | Ported | `RunPolicy.RetryAttempts`, `IsRetryableAssistantError` | Non-retryable auth failures fail fast. |
| Queued messages (steer/follow-up) | Ported | `Steer`, `FollowUp` | Delivered at turn boundaries. |
| Cancellation | Ported | context cancellation, `Abort` | Reaches provider I/O, tools and child processes. |

## Tools and resources

| Pi area | Status | Go surface | Notes |
| --- | --- | --- | --- |
| Built-in read/write/edit/bash | Ported | `CreateReadTool`, `CreateWriteTool`, `CreateEditTool`, `CreateBashTool` | Reuses the accepted harness tools. |
| grep/find/ls | Ported | `CreateGrepTool`, `CreateFindTool`, `CreateLsTool` | Go implementations with truncation. |
| Custom tools | Adapted | `ToolDefinition`, `ToolRegistry` | Constructed directly in Go instead of `defineTool` in TS. |
| Tool hooks / validation | Ported | `ToolHooks`, `ToolRegistry.Execute` | `Before` runs before execution and can deny. |
| File mutation queue | Ported | `WithFileMutationQueue` | Serializes same-path mutations. |
| Output truncation | Ported | `TruncateHead`, `TruncateTail`, `TruncateLine` | Explicit limits; never silently drops capacity. |
| Skills (plain text) | Ported | `LoadSkills`, `Skill`, `FormatSkillsForPrompt` | No JS skill execution. |
| Prompt templates | Ported | `LoadPromptTemplates`, `ExpandTemplate` | Plain markdown plus frontmatter. |
| Project context files | Ported | `LoadResources`, `ContextFile` | Explicit paths only; no implicit home discovery. |
| System prompt assembly | Ported | `BuildSystemPrompt` | Headless prompt text. |

## Configuration and settings

| Pi area | Status | Go surface | Notes |
| --- | --- | --- | --- |
| Layered settings | Ported | `LoadSettings`, `SaveSettings`, `SettingsManager` | Unknown JSON keys survive round trips. |
| Credential storage | Ported | `AuthStorage`, `FileAuthStorageBackend` | Headless file-backed credentials. |
| Provider/auth helpers | Ported | `ResolveConfigValue`, `AuthStatus` | Env and command config values. |
| HTTP dispatcher/proxy | Ported | `ConfigureHTTPDispatcher`, `ApplyHTTPProxySettings` | Applies to provider requests. |

## Excluded from this increment

| Pi area | Status | Reason |
| --- | --- | --- |
| TS/JS extension execution | Excluded | Custom Go tools and hooks replace extension plugins. |
| `ExtensionAPI` / `ExtensionContext` / `ExtensionRunner` | Excluded | No JS extension host is shipped. |
| Extension lifecycle hook events (`BeforeAgentStartEvent`, `SessionBefore*Event`, provider hooks) | Excluded | Go hooks cover tool execution; extension hook events have no headless equivalent. |
| TUI widgets, themes, editors, renderers | Excluded | This SDK is headless; terminal rendering is out of scope. |
| `RpcClient` / RPC host mode | Excluded | Remote service management is out of scope. |
| npm/Git extension package manager | Excluded | Package installation is explicitly out of scope. |
| Browser login / OAuth UI | Excluded | Headless credential callbacks are provided instead. |
| Clipboard, image resize/PNG, syntax highlighting | Excluded | TUI-only utilities. |
| CLI orchestration (`main`, `parseArgs`, print/interactive modes) | Excluded | Stays in `cmd/pith`; the SDK is a library. |

## Known adaptation caveats

- The TS event union is collapsed into `SessionEvent`; event kinds that are not
  in the headless subset (session tree/compact/shutdown notifications) are not
  republished. Use `SessionManager` and `Compact` for those states.
- Source-map `reason` fields record per-export adaptations; when an export mixes
  excluded host behavior with SDK behavior, the SDK part is implemented and the
  host part is excluded rather than claimed as parity.
- The SDK is not a filesystem sandbox or an authorization system. Tool hooks are
  application policy, not a security boundary.
- Provider coverage depends on the accepted Pith AI packages; this table does
  not assert a level of behavioral parity beyond the tested contract.
