// Public entry points of the embedded coding-agent SDK.
//
// This file ports the construction half of
// packages/coding-agent/src/core/sdk.ts, agent-session-services.ts and
// agent-session-runtime.ts from Pi at revision
// f07218c4d4bbc12bef056a7058c3dd49dfe41abe.
//
// The frozen integration surface is CreateAgentSession plus SessionOptions,
// RunPolicy, SessionEvent and RunResult. The runtime/services helpers mirror
// the upstream split between cwd-bound services and session creation so callers
// can resolve a model, load resources and build a tool registry before creating
// a session. TS extension packages, project-trust prompts, remote service
// management and interactive login are excluded from this increment.
//
// Upstream: Copyright (c) 2025 Mario Zechner, MIT License.
// See the repository LICENSE for the full text.
package codingagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	agenttypes "github.com/minifish-org/pith/packages/agent/types"
	aitypes "github.com/minifish-org/pith/packages/ai/types"
)

// SessionOptions configures CreateAgentSession. All inputs are explicit:
// nothing is discovered from a process-global location.
type SessionOptions struct {
	// Cwd is the project working directory used for resources and tools.
	Cwd string
	// Model selects the provider model plus its per-caller capabilities.
	Model ModelOptions
	// Manager is the durable session tree. Nil creates an in-memory manager
	// that the session owns and closes.
	Manager *SessionManager
	// Resources are the explicit skill/template/context/prompt inputs.
	Resources ResourceOptions
	// Tools is the prepared tool registry. Nil builds the default registry.
	Tools *ToolRegistry
	// Settings is the merged settings snapshot.
	Settings Settings
	// Policy bounds retries, turns and compaction.
	Policy RunPolicy

	// ownsManager is set when CreateAgentSession creates the manager itself.
	ownsManager bool
}

// RunPolicy bounds a session's runtime behavior. Every field is opt-in: the
// zero value imposes no cap and compact summarizes nothing.
type RunPolicy struct {
	// MaxTurns ends a run after this many assistant turns. Zero is unlimited.
	MaxTurns int
	// RetryAttempts retries a transient model error this many times.
	RetryAttempts int
	// RetryDelay is the pause between retry attempts.
	RetryDelay time.Duration
	// CompactReserveTokens triggers auto-compaction when the estimated context
	// plus this reserve exceeds the model context window. Zero disables it.
	CompactReserveTokens int
	// KeepRecentMessages bounds the verbatim tail retained by Compact.
	KeepRecentMessages int
	// Summarize folds older messages into one summary string. Required by
	// Compact and by auto-compaction.
	Summarize func(ctx context.Context, messages []agenttypes.AgentMessage) (string, error)
}

// AgentSessionRuntimeDiagnostic is one setup warning or error.
type AgentSessionRuntimeDiagnostic struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
}

// AgentSessionServices are the cwd-bound building blocks of one session.
type AgentSessionServices struct {
	Cwd       string
	AgentDir  string
	Manager   *SessionManager
	Tools     *ToolRegistry
	Resources ResourceSet
	Settings  Settings
}

// CreateAgentSessionServicesOptions configures CreateAgentSessionServices.
type CreateAgentSessionServicesOptions struct {
	Cwd       string
	AgentDir  string
	Manager   *SessionManager
	Resources ResourceOptions
	Tools     *ToolRegistry
	Settings  Settings
}

// CreateAgentSessionFromServicesOptions configures
// CreateAgentSessionFromServices.
type CreateAgentSessionFromServicesOptions struct {
	Services AgentSessionServices
	Model    ModelOptions
	Policy   RunPolicy
	Settings Settings
}

// CreateAgentSessionRuntimeResult is returned by a runtime factory.
type CreateAgentSessionRuntimeResult struct {
	Session              *AgentSession
	Services             AgentSessionServices
	Diagnostics          []AgentSessionRuntimeDiagnostic
	ModelFallbackMessage *string
}

// CreateAgentSessionRuntimeOptions is the input of a runtime factory.
type CreateAgentSessionRuntimeOptions struct {
	Cwd       string
	AgentDir  string
	Manager   *SessionManager
	Resources ResourceOptions
}

// CreateAgentSessionRuntimeFactory builds a full session runtime for a cwd.
type CreateAgentSessionRuntimeFactory func(options CreateAgentSessionRuntimeOptions) (CreateAgentSessionRuntimeResult, error)

// SessionImportFileNotFoundError reports a missing import path.
type SessionImportFileNotFoundError struct {
	FilePath string
}

// Error implements the error interface.
func (e *SessionImportFileNotFoundError) Error() string {
	return "File not found: " + e.FilePath
}

// CreateAgentSessionServices loads cwd-bound resources and opens the session
// manager. It does not create an AgentSession.
func CreateAgentSessionServices(options CreateAgentSessionServicesOptions) (AgentSessionServices, error) {
	cwd := options.Cwd
	agentDir := options.AgentDir

	if options.Manager == nil {
		manager, err := OpenSession("")
		if err != nil {
			return AgentSessionServices{}, err
		}
		options.Manager = manager
	}

	resources := options.Resources
	if strings.TrimSpace(resources.Cwd) == "" {
		resources.Cwd = cwd
	}
	if strings.TrimSpace(resources.AgentDir) == "" {
		resources.AgentDir = agentDir
	}
	resourceSet, err := LoadResources(resources)
	if err != nil {
		return AgentSessionServices{}, err
	}

	tools := options.Tools
	if tools == nil {
		registry, err := NewToolRegistry(cwd, nil, nil, nil, ToolHooks{})
		if err != nil {
			return AgentSessionServices{}, err
		}
		tools = registry
	}

	return AgentSessionServices{
		Cwd:       cwd,
		AgentDir:  agentDir,
		Manager:   options.Manager,
		Tools:     tools,
		Resources: resourceSet,
		Settings:  options.Settings,
	}, nil
}

// CreateAgentSessionFromServices builds an AgentSession from prepared
// services.
func CreateAgentSessionFromServices(options CreateAgentSessionFromServicesOptions) (*AgentSession, error) {
	services := options.Services
	resourceOptions := ResourceOptions{
		Cwd:      services.Cwd,
		AgentDir: services.AgentDir,
	}
	return CreateAgentSession(SessionOptions{
		Cwd:       services.Cwd,
		Model:     options.Model,
		Manager:   services.Manager,
		Resources: resourceOptions,
		Tools:     services.Tools,
		Settings:  options.Settings,
		Policy:    options.Policy,
	})
}

// CreateAgentSessionRuntime invokes the factory and wraps the result.
func CreateAgentSessionRuntime(factory CreateAgentSessionRuntimeFactory, options CreateAgentSessionRuntimeOptions) (*AgentSessionRuntime, error) {
	if factory == nil {
		return nil, fmt.Errorf("create runtime: factory is required")
	}
	if options.Manager == nil {
		manager, err := OpenSession("")
		if err != nil {
			return nil, err
		}
		options.Manager = manager
	}
	if err := AssertSessionCwdExists(options.Manager, options.Cwd); err != nil {
		return nil, err
	}
	result, err := factory(options)
	if err != nil {
		return nil, err
	}
	return &AgentSessionRuntime{
		session:     result.Session,
		services:    result.Services,
		diagnostics: result.Diagnostics,
		fallback:    result.ModelFallbackMessage,
		factory:     factory,
	}, nil
}

// AgentSessionRuntime owns a session plus its cwd-bound services.
type AgentSessionRuntime struct {
	session     *AgentSession
	services    AgentSessionServices
	diagnostics []AgentSessionRuntimeDiagnostic
	fallback    *string
	factory     CreateAgentSessionRuntimeFactory
}

// Session returns the current session.
func (r *AgentSessionRuntime) Session() *AgentSession { return r.session }

// Services returns the cwd-bound services.
func (r *AgentSessionRuntime) Services() AgentSessionServices { return r.services }

// Cwd returns the runtime working directory.
func (r *AgentSessionRuntime) Cwd() string { return r.services.Cwd }

// Diagnostics returns the setup diagnostics.
func (r *AgentSessionRuntime) Diagnostics() []AgentSessionRuntimeDiagnostic {
	return append([]AgentSessionRuntimeDiagnostic(nil), r.diagnostics...)
}

// ModelFallbackMessage reports that the saved model could not be restored.
func (r *AgentSessionRuntime) ModelFallbackMessage() *string {
	return r.fallback
}

// Close closes the owned session.
func (r *AgentSessionRuntime) Close() error {
	if r.session == nil {
		return nil
	}
	return r.session.Close()
}

// ResolveSessionModel applies explicit overrides over the model itself. It is a
// thin convenience around ResolveModel that also validates the thinking level.
func ResolveSessionModel(options ModelOptions) (*aitypes.Model, agenttypes.ThinkingLevel, error) {
	model, err := ResolveModel(options)
	if err != nil {
		return nil, agenttypes.ThinkingOff, err
	}
	level := options.ThinkingLevel
	if level == "" {
		level = DefaultThinkingLevel
	}
	return model, level, nil
}
