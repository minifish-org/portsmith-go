# Pith embedded SDK

`github.com/minifish-org/pith/packages/coding-agent` (package `codingagent`) is
the headless, in-process Go SDK for the Pith coding agent. It is an additive Go
port of the Pi coding-agent core at revision
`f07218c4d4bbc12bef056a7058c3dd49dfe41abe` and reuses the accepted Pith AI,
agent-core, harness, session/compaction and tool packages as dependencies.

The SDK embeds the agent loop directly in a Go process. There is no subprocess,
no Node or Pi binary and no IPC boundary between the application and the agent.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"os"

	aitypes "github.com/minifish-org/pith/packages/ai/types"
	codingagent "github.com/minifish-org/pith/packages/coding-agent"
)

func main() {
	model := &aitypes.Model{
		Id: "gpt-4o-mini", Name: "gpt-4o-mini",
		Api: aitypes.ApiOpenAICompletions, Provider: aitypes.ProviderOpenAI,
		BaseUrl: "https://api.openai.com/v1",
		ContextWindow: 128000, MaxTokens: 16384,
	}
	session, err := codingagent.CreateAgentSession(codingagent.SessionOptions{
		Cwd: "/path/to/project",
		Model: codingagent.ModelOptions{
			Model: model,
			APIKey: func(context.Context, string) (string, error) {
				return os.Getenv("OPENAI_API_KEY"), nil
			},
		},
	})
	if err != nil {
		panic(err)
	}
	defer session.Close()

	result, err := session.Prompt(context.Background(), "fix the failing test")
	if err != nil {
		panic(err)
	}
	fmt.Println(result.StopReason)
}
```

Runnable examples live in `examples/embedded-sdk/`:

| Example | Demonstrates |
| --- | --- |
| `minimal` | one prompt with an offline fake stream or a live provider |
| `custom-tool` | a custom Go tool, execution hooks and a credential callback |
| `events` | subscribing to the structured session event stream |
| `resume` | durable sessions, branch/leaf state and reopen |
| `resources` | skills, prompt templates and project context files |
| `web` | HTTP/SSE embedding, cancellation and per-user session isolation |

All examples default to offline fake streams. Live providers require explicit
environment configuration (`PITH_SDK_LIVE=1` plus `OPENAI_API_KEY`). `go test`
never calls a paid model.

## Public package

The single supported import is:

```
github.com/minifish-org/pith/packages/coding-agent
```

The frozen integration surface is:

- `CreateAgentSession(SessionOptions) (*AgentSession, error)`
- `SessionOptions`, `ModelOptions`, `RunPolicy`, `ToolHooks`
- `AgentSession.Prompt`, `.Subscribe`, `.Messages`, `.SetModel`,
  `.SetActiveTools`, `.Steer`, `.FollowUp`, `.Abort`, `.Compact`, `.Stats`,
  `.Close`
- `ToolDefinition`, `ToolResult`, `ToolCall`, `ToolRegistry`
- `SessionManager`, `SessionEntry` and the append/branch API
- `LoadSettings`/`SaveSettings`, `LoadResources`, `ExpandTemplate`
- `SessionEvent` and `RunResult`

Helper constructors (`CreateAgentSessionServices`,
`CreateAgentSessionFromServices`, `CreateAgentSessionRuntime`) mirror the
upstream split between cwd-bound services and session creation so an
application can resolve a model, load resources and build a tool registry
before creating a session. They are optional; `CreateAgentSession` builds the
same pieces itself.

## Lifecycle and ownership

`CreateAgentSession` owns everything it creates:

- When `SessionOptions.Manager` is nil the session opens its own manager (an
  in-memory one when no file is given) and closes it in `Close`.
- When the caller supplies a `*SessionManager`, the caller owns and closes it.
- The caller owns the context passed to `Prompt`, the credential callback, the
  tool registry it supplies, and every custom tool implementation.

`Close` is safe to call more than once. A session is single-run: `Prompt`
returns `ErrAgentSessionBusy` while another run is active, and `SetModel` /
`SetActiveTools` are rejected during a run. After a completed, failed or
cancelled run the session remains usable; the next `Prompt` continues from the
durable transcript.

## Concurrency

One `AgentSession` serializes `Prompt`. `Subscribe`, `Steer`, `FollowUp`,
`Abort`, `Messages`, `Stats`, `SetModel` and `SetActiveTools` are safe to call
from other goroutines. `Subscribe` callbacks run outside the session lock, so a
listener may inspect snapshots or unsubscribe without deadlocking. There is no
package-global model registry and no process-wide default stream function, so
two sessions in the same process do not interfere.

## Providers and credentials

`ModelOptions.Model` is a `*aitypes.Model` from the accepted Pith AI catalog.
When `ModelOptions.StreamFn` is nil the SDK resolves the native provider for the
model's `Api` through the existing Pith AI API modules:

| API id | Provider surface |
| --- | --- |
| `openai-completions` | OpenAI Chat Completions and OpenAI-compatible endpoints |
| `openai-responses` | OpenAI Responses |
| `openai-codex-responses` | OpenAI Codex Responses |
| `azure-openai-responses` | Azure OpenAI Responses |
| `anthropic-messages` | Anthropic Messages |
| `google-generative-ai` | Google Generative AI |
| `google-vertex` | Google Vertex |
| `mistral-conversations` | Mistral Conversations |
| `bedrock-converse-stream` | Amazon Bedrock Converse |
| `pi-messages` | Pi Messages |

The model's `BaseUrl`, capacity (`ContextWindow`/`MaxTokens`), compatibility
flags and headers are passed through unchanged. `ModelOptions.APIKey` runs on
every request and receives the provider id; it is never cached globally.
`ModelOptions.MaxTokens`/`ContextWindow` are explicit overrides and are
rejected when they exceed the model's own capacity, so capacity is never
silently reduced. A caller that needs a custom transport sets `StreamFn`
directly.

## Cancellation

Cancellation is context based. Passing a cancelled context to `Prompt` cancels
the provider HTTP request, running tools and child processes started by the
bash tool. `Abort` cancels the active run without a context and also interrupts
retry backoff. A cancelled run terminates with the aborted stop reason; the
session stays usable.

## Limits

The SDK imposes no implicit per-run turn, time, file-size or repair cap. Every
budget is explicit:

- `RunPolicy.MaxTurns` bounds assistant turns.
- `RunPolicy.RetryAttempts` and `RetryDelay` bound transient retries.
- `RunPolicy.CompactReserveTokens` triggers automatic compaction.
- `RunPolicy.KeepRecentMessages` bounds the verbatim tail after compaction.
- `RunPolicy.Summarize` supplies the summary function.

Length-truncated assistant responses are continued rather than treated as
complete, and an incomplete tool-argument payload is never executed.

## Privacy

The SDK reads only the paths named by `ResourceOptions` and the project working
directory required by the enabled tools. It does not upload files by itself and
does not read the user's home directory implicitly. Credential callbacks and
prompts are the caller's responsibility. Errors exposed by the SDK are
sanitized diagnostics and do not include credential values. The bundled web
example demonstrates embedding only and is not an authentication system.

## Failures

Provider and tool failures surface as Go errors or encoded in the assistant
message stop reason. Tool calls and tool results are always paired in the
persisted transcript and turn boundaries are durable. Retry exhaustion is
reported to the caller. A tool `Before` hook runs before every executable call
and can deny it; `After` hooks may transform results.

## Migration from the old `sdk.Run`

The convenience loop `github.com/minifish-org/pith/packages/agent/sdk.Run` is
unchanged in this increment and still works for a single OpenAI Chat
Completions conversation. New code should migrate to the embedded session:

```go
// old convenience loop
// err := agentsdk.Run(agentsdk.RunOptions{BaseURL: ..., Model: ..., APIKey: ..., Prompt: ...})

// embedded session
session, err := codingagent.CreateAgentSession(codingagent.SessionOptions{
	Cwd: cwd,
	Model: codingagent.ModelOptions{Model: model, APIKey: keyCallback},
})
defer session.Close()
result, err := session.Prompt(ctx, prompt)
```

The session entry point adds a durable transcript, typed events, tool
registry/hooks, resource loading, per-model provider resolution, explicit
budgets, compaction and cancellation, none of which are part of the old loop.

## Exclusions

This increment is headless. TS/JS extension execution, JS execution plugins,
TUI widgets and themes, npm/Git extension package installation, remote service
management, RPC host mode and browser login UI are out of scope. Plain-text
skills, project context files and prompt templates remain resources. Custom Go
tools and `ToolHooks` replace JS execution plugins, and credential callbacks
plus provider selection are in scope. See
[compatibility.md](./compatibility.md) for the frozen table and the caveat that
passing tests is evidence for the covered contract, not proof of full Pi
parity.
