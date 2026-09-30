// Embedded SDK agent session.
//
// This file ports the session half of
// packages/coding-agent/src/core/agent-session.ts, agent-session-services.ts
// and agent-session-runtime.ts from Pi at revision
// f07218c4d4bbc12bef056a7058c3dd49dfe41abe onto the accepted Pith Agent and
// agent loop.
//
// Unlike the standalone sdk.Run convenience loop, an AgentSession owns a real
// agent.Agent: it assembles the resolved model/options, resources, tool
// registry, settings and durable session manager, then runs the accepted agent
// loop synchronously. Every lifecycle event is published as a SessionEvent and
// every completed message is appended to the session tree before the next
// request. The upstream TUI, JS extension runner, slash commands, cache warmer
// and interactive login flows are excluded from this headless increment.
//
// Upstream: Copyright (c) 2025 Mario Zechner, MIT License.
// See the repository LICENSE for the full text.
package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	agentcore "github.com/minifish-org/pith/packages/agent"
	agenttypes "github.com/minifish-org/pith/packages/agent/types"
	aitypes "github.com/minifish-org/pith/packages/ai/types"
	aiutils "github.com/minifish-org/pith/packages/ai/utils"
)

// ErrAgentSessionClosed is returned by mutations after Close.
var ErrAgentSessionClosed = errors.New("agent session is closed")

// ErrAgentSessionBusy is returned when a second Prompt or a configuration
// change races an active run.
var ErrAgentSessionBusy = errors.New("agent session is already running")

// errNoSummarizer is returned by Compact without a configured summarizer.
var errNoSummarizer = errors.New("compaction requires RunPolicy.Summarize")

// ErrAgentAborted reports a run that stopped because Abort or caller
// cancellation was observed. It is distinct from a provider failure.
var ErrAgentAborted = errors.New("agent run aborted")

// TerminalRequestError wraps a provider failure that exhausted retries or is
// not retryable. The error text is preserved without credentials.
type TerminalRequestError struct {
	Message    string
	StopReason aitypes.StopReason
}

// Error implements the error interface.
func (e *TerminalRequestError) Error() string {
	if e.Message == "" {
		return "model request failed"
	}
	return "model request failed: " + e.Message
}

// Unwrap lets errors.Is/As reach a wrapped context error.
func (e *TerminalRequestError) Unwrap() error { return nil }

// defaultSessionSystemPrompt is the fallback instruction when resource loading
// yields no prompt.
const defaultSessionSystemPrompt = "You are a coding agent. Use the provided tools to complete the user's task."

// sessionListenerEntry is one subscriber with a stable removal id.
type sessionListenerEntry struct {
	id       uint64
	listener func(SessionEvent)
}

// runCollector accumulates the detached outcome of one synchronous run.
type runCollector struct {
	messages []agenttypes.AgentMessage
	usage    aitypes.Usage
	turns    int
	stop     aitypes.StopReason
}

func (r *runCollector) addMessage(message agenttypes.AgentMessage) {
	r.messages = append(r.messages, message)
}

func (r *runCollector) addUsage(usage aitypes.Usage) {
	r.usage.Input += usage.Input
	r.usage.Output += usage.Output
	r.usage.CacheRead += usage.CacheRead
	r.usage.CacheWrite += usage.CacheWrite
	r.usage.Cost.Input += usage.Cost.Input
	r.usage.Cost.Output += usage.Cost.Output
	r.usage.Cost.CacheRead += usage.Cost.CacheRead
	r.usage.Cost.CacheWrite += usage.Cost.CacheWrite
	r.usage.Cost.Total += usage.Cost.Total
	r.usage.TotalTokens += usage.TotalTokens
}

// AgentSession is a single embedded coding-agent conversation.
type AgentSession struct {
	mu         sync.Mutex
	closed     bool
	running    bool
	compacting bool

	// runCancel cancels the active Prompt (including retry backoff);
	// abortRequested records an explicit Abort so the retry loop stops.
	runCancel      context.CancelFunc
	abortRequested bool
	maxTurnsHit    bool

	cwd         string
	manager     *SessionManager
	registry    *ToolRegistry
	resources   ResourceSet
	settings    Settings
	policy      RunPolicy
	model       *aitypes.Model
	modelConfig ModelOptions
	thinking    agenttypes.ThinkingLevel
	ownsManager bool

	agent            *agentcore.Agent
	unsubscribeAgent func()

	listeners      []sessionListenerEntry
	nextListenerID uint64

	run *runCollector

	lastAssistantEntryID string
}

// AgentSessionConfig is the Go adaptation of the upstream AgentSessionConfig.
// The headless SDK derives the agent, model runtime and extension runner
// internally, so the config is the same shape as SessionOptions.
type AgentSessionConfig = SessionOptions

// CreateAgentSession assembles and returns a headless AgentSession.
func CreateAgentSession(options SessionOptions) (*AgentSession, error) {
	if options.Manager == nil {
		manager, err := OpenSession("")
		if err != nil {
			return nil, err
		}
		options.Manager = manager
		options.ownsManager = true
	}

	cwd := strings.TrimSpace(options.Cwd)
	if cwd == "" {
		cwd = options.Manager.GetCwd()
	}

	resources := options.Resources
	if strings.TrimSpace(resources.Cwd) == "" {
		resources.Cwd = cwd
	}
	resourceSet, err := LoadResources(resources)
	if err != nil {
		if options.ownsManager {
			_ = options.Manager.Close()
		}
		return nil, fmt.Errorf("load resources: %w", err)
	}

	if options.Tools == nil {
		registry, err := NewToolRegistry(cwd, nil, nil, nil, ToolHooks{})
		if err != nil {
			if options.ownsManager {
				_ = options.Manager.Close()
			}
			return nil, err
		}
		options.Tools = registry
	}

	model, err := ResolveModel(options.Model)
	if err != nil {
		if options.ownsManager {
			_ = options.Manager.Close()
		}
		return nil, err
	}

	thinking := options.Model.ThinkingLevel
	if thinking == "" {
		thinking = DefaultThinkingLevel
	}

	session := &AgentSession{
		cwd:         cwd,
		manager:     options.Manager,
		registry:    options.Tools,
		resources:   resourceSet,
		settings:    options.Settings,
		policy:      options.Policy,
		model:       model,
		modelConfig: options.Model,
		thinking:    thinking,
		ownsManager: options.ownsManager,
	}

	session.mu.Lock()
	err = session.rebuildAgentLocked()
	session.mu.Unlock()
	if err != nil {
		if options.ownsManager {
			_ = options.Manager.Close()
		}
		return nil, err
	}
	return session, nil
}

// rebuildAgentLocked builds a fresh agent from the durable session context and
// swaps it in. The caller must hold s.mu and ensure no run is active.
func (s *AgentSession) rebuildAgentLocked() error {
	built, unsubscribe, err := s.buildAgentLocked()
	if err != nil {
		return err
	}
	if s.unsubscribeAgent != nil {
		s.unsubscribeAgent()
	}
	s.agent = built
	s.unsubscribeAgent = unsubscribe
	return nil
}

func (s *AgentSession) buildAgentLocked() (*agentcore.Agent, func(), error) {
	messages := s.manager.BuildSessionContext().Messages

	systemPrompt := strings.TrimSpace(s.resources.SystemPrompt)
	if systemPrompt == "" {
		systemPrompt = defaultSessionSystemPrompt
	}

	modelConfig := s.modelConfig
	streamFn := modelConfig.StreamFn
	if streamFn == nil {
		// Callers may omit StreamFn and rely on the native provider for the
		// resolved model's API, as upstream createAgentSession does. The
		// fallback is bound to this session's model and is not process-global.
		streamFn = builtinStreamFnForModel(s.model)
	}
	apiKey := modelConfig.APIKey

	getAPIKey := func(provider string) (string, bool, error) {
		if apiKey == nil {
			return "", false, nil
		}
		key, err := apiKey(context.Background(), provider)
		if err != nil {
			return "", false, err
		}
		return key, key != "", nil
	}

	options := agentcore.AgentOptions{
		InitialState: &agentcore.AgentInitialState{
			SystemPrompt:  &systemPrompt,
			Model:         s.model,
			ThinkingLevel: s.thinking,
			Tools:         s.registry.AgentTools(),
			Messages:      messages,
		},
		ConvertToLlm: ConvertToLlm,
		StreamFn:     streamFn,
		GetApiKey:    getAPIKey,
		SessionId:    s.manager.SessionID(),
		FinishTurn:   s.finishTurn,
	}
	built, err := agentcore.NewAgent(options)
	if err != nil {
		return nil, nil, err
	}
	unsubscribe := built.Subscribe(s.handleAgentEvent)
	return built, unsubscribe, nil
}

// finishTurn enforces an explicit RunPolicy.MaxTurns budget and requests a
// continuation for a length-truncated assistant response. A zero MaxTurns means
// no implicit cap. A length stop is never treated as a complete answer: the
// loop is asked to continue until a non-length response or caller cancellation.
func (s *AgentSession) finishTurn(turn agenttypes.AgentTurnContext, _ <-chan struct{}) (agenttypes.AgentTurnDecision, error) {
	if turn.Message.StopReason == aitypes.StopReasonLength {
		return agenttypes.AgentTurnDecision{Action: "continue"}, nil
	}

	s.mu.Lock()
	maxTurns := s.policy.MaxTurns
	run := s.run
	if maxTurns > 0 && run != nil && run.turns+1 >= maxTurns {
		s.maxTurnsHit = true
		s.mu.Unlock()
		return agenttypes.AgentTurnDecision{Action: "end"}, nil
	}
	s.mu.Unlock()
	return agenttypes.AgentTurnDecision{}, nil
}

// handleAgentEvent is the single agent listener. It persists completed
// messages, collects run statistics and publishes the public SessionEvent. It
// never calls back into the agent, so the agent's event lock is not reentered.
func (s *AgentSession) handleAgentEvent(event agenttypes.AgentEvent, _ <-chan struct{}) error {
	s.mu.Lock()
	run := s.run
	s.mu.Unlock()

	switch event.Type {
	case agenttypes.AgentEventMessageEnd:
		if event.Message != nil {
			if run != nil {
				run.addMessage(cloneAgentMessage(*event.Message))
				if assistant, ok := assistantMessageOf(event.Message); ok {
					run.addUsage(assistant.Usage)
					run.stop = assistant.StopReason
				}
			}
			s.persistMessage(event.Message)
		}
	case agenttypes.AgentEventTurnEnd:
		if run != nil {
			run.turns++
			if assistant, ok := assistantMessageOf(event.Message); ok && assistant.StopReason != "" {
				run.stop = assistant.StopReason
			}
		}
	}

	s.publish(sessionEventFromAgentEvent(event))
	return nil
}

// persistMessage appends a completed non-system message to the session tree.
func (s *AgentSession) persistMessage(message *agenttypes.AgentMessage) {
	if message == nil {
		return
	}
	if message.Message != nil && message.Message.Role == aitypes.SystemMessageRole {
		return
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return
	}
	entry, err := s.manager.AppendMessage(raw)
	if err != nil {
		return
	}
	if assistant, ok := assistantMessageOf(message); ok && assistant.StopReason != "" {
		s.mu.Lock()
		s.lastAssistantEntryID = entry.ID
		s.mu.Unlock()
	}
}

// publish dispatches an event to a stable copy of the subscriber list. User
// listeners run outside the session lock so they can inspect snapshots and
// unsubscribe without deadlocking.
func (s *AgentSession) publish(event SessionEvent) {
	s.mu.Lock()
	listeners := make([]sessionListenerEntry, len(s.listeners))
	copy(listeners, s.listeners)
	s.mu.Unlock()
	for _, entry := range listeners {
		func() {
			defer func() { _ = recover() }()
			entry.listener(event)
		}()
	}
}

// sessionEventFromAgentEvent maps the agent lifecycle event onto the stable
// public session event.
//
// Partial message lifecycle events (message_start, message_update) carry a
// pointer to the assistant message that the provider goroutine is still
// appending to. Reading its content here would race with the provider, so only
// terminal events copy the finalized message; streaming updates expose the
// incremental delta string, which is immutable.
func sessionEventFromAgentEvent(event agenttypes.AgentEvent) SessionEvent {
	out := SessionEvent{Type: event.Type}
	if event.ToolName != nil {
		out.ToolName = *event.ToolName
	}
	if event.ToolCallId != nil {
		out.ToolCallID = *event.ToolCallId
	}
	if event.IsError != nil {
		out.IsError = *event.IsError
	}
	switch event.Type {
	case agenttypes.AgentEventMessageEnd, agenttypes.AgentEventTurnEnd,
		agenttypes.AgentEventToolExecutionEnd, agenttypes.AgentEventAgentEnd:
		if event.Message != nil {
			out.Message = event.Message
			out.Text = messageText(*event.Message)
		}
	case agenttypes.AgentEventMessageUpdate:
		if event.AssistantMessageEvent != nil && event.AssistantMessageEvent.Delta != nil {
			out.Text = *event.AssistantMessageEvent.Delta
		}
	}
	return out
}

// SubscriberCount reports the number of active subscribers. It is used by
// diagnostics and tests that verify listener cleanup.
func (s *AgentSession) SubscriberCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.listeners)
}

// Subscribe registers a listener and returns an idempotent unsubscribe.
func (s *AgentSession) Subscribe(listener func(SessionEvent)) func() {
	if listener == nil {
		return func() {}
	}
	s.mu.Lock()
	id := s.nextListenerID
	s.nextListenerID++
	s.listeners = append(s.listeners, sessionListenerEntry{id: id, listener: listener})
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			out := s.listeners[:0]
			for _, entry := range s.listeners {
				if entry.id != id {
					out = append(out, entry)
				}
			}
			s.listeners = out
		})
	}
}

// Prompt runs one synchronous conversation turn (and every tool continuation)
// until the agent stops. A cancelled context is rejected before any request.
//
// Resilience behavior:
//   - No implicit turn or wall-clock cap. An explicit MaxTurns > 0 ends the run
//     with ErrMaxTurnsExceeded and a persisted transcript.
//   - A length-truncated assistant response never executes its tool calls and
//     the run is continued until a non-length response or cancellation.
//   - A transient 429/5xx/connection failure is retried with cancellable
//     backoff. RetryAttempts counts extra requests; zero selects the default
//     three and a negative value disables retries. Auth/invalid-request
//     failures are never retried. A completed tool effect is never replayed.
func (s *AgentSession) Prompt(ctx context.Context, text string) (RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return RunResult{}, err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return RunResult{}, ErrAgentSessionClosed
	}
	if s.running || s.compacting {
		s.mu.Unlock()
		return RunResult{}, ErrAgentSessionBusy
	}
	s.running = true
	s.abortRequested = false
	s.maxTurnsHit = false
	runCtx, cancel := context.WithCancel(ctx)
	s.runCancel = cancel
	s.mu.Unlock()

	defer func() {
		cancel()
		s.mu.Lock()
		s.running = false
		s.runCancel = nil
		s.mu.Unlock()
	}()

	// Auto-compaction is opt-in through a summarizer plus a reserve. It runs
	// before the prompt so the request starts from a bounded context.
	if err := s.maybeCompact(runCtx); err != nil {
		return RunResult{}, err
	}

	retries := effectiveRetryAttempts(s.policy.RetryAttempts)
	promptText := text
	usePrompt := true
	for attempt := 0; ; {
		result, err := s.runOnce(runCtx, promptText, usePrompt)
		if err != nil {
			return result, err
		}
		if runCtx.Err() != nil {
			return result, runCtx.Err()
		}
		if s.isAbortRequested() {
			return result, ErrAgentAborted
		}
		if s.takeMaxTurnsHit() {
			return result, ErrMaxTurnsExceeded
		}
		// An aborted turn is terminal and distinct from a provider error.
		if result.StopReason == aitypes.StopReasonAborted {
			if runCtx.Err() != nil {
				return result, runCtx.Err()
			}
			return result, ErrAgentAborted
		}
		if result.StopReason != aitypes.StopReasonError {
			return result, nil
		}

		// Terminal provider failure: classify retryability before spending a
		// retry budget. Auth and invalid-request failures fail fast.
		assistant, ok := lastAssistantMessage(result.Messages)
		if !ok {
			return result, &TerminalRequestError{StopReason: result.StopReason}
		}
		if !aiutils.IsRetryableAssistantError(assistant) {
			return result, terminalErrorFor(assistant)
		}
		if attempt >= retries {
			return result, terminalErrorFor(assistant)
		}

		// Preserve coherent raw history while omitting the failed attempt from
		// the model projection, then retry as a continuation.
		if err := s.omitFailedAssistant(); err != nil {
			return result, terminalErrorFor(assistant)
		}
		if err := s.rebuild(); err != nil {
			return result, err
		}
		attempt++
		s.emitRetryStart(attempt, retries, assistant)
		if err := sleepContext(runCtx, s.retryDelayFor(attempt)); err != nil {
			s.emitRetryEnd(false, attempt, err)
			if runCtx.Err() != nil {
				return result, runCtx.Err()
			}
			return result, ErrAgentAborted
		}
		s.emitRetryEnd(true, attempt, nil)
		usePrompt = false
	}
}

// ErrMaxTurnsExceeded reports that an explicit RunPolicy.MaxTurns ended a run.
var ErrMaxTurnsExceeded = errors.New("maximum turns reached")

// runOnce performs one synchronous agent run and returns the detached result.
// The caller (Prompt) owns the running slot so retries and auto-compaction stay
// serialized against other mutations.
func (s *AgentSession) runOnce(ctx context.Context, text string, usePrompt bool) (RunResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return RunResult{}, ErrAgentSessionClosed
	}
	run := &runCollector{}
	s.run = run
	activeAgent := s.agent
	s.mu.Unlock()

	finished := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			if activeAgent != nil {
				activeAgent.Abort()
			}
		case <-finished:
		}
	}()

	var err error
	if activeAgent == nil {
		err = ErrAgentSessionClosed
	} else if usePrompt {
		err = activeAgent.PromptString(text, nil)
	} else {
		err = activeAgent.Continue()
	}
	close(finished)
	<-watcherDone

	result := RunResult{}
	s.mu.Lock()
	result.StopReason = run.stop
	result.Turns = run.turns
	result.Messages = cloneAgentMessages(run.messages)
	result.Usage = run.usage
	s.run = nil
	s.mu.Unlock()

	return result, err
}

// omitFailedAssistant removes the most recent assistant entry from the active
// branch so a retry re-issues the request without replaying the failed turn.
func (s *AgentSession) omitFailedAssistant() error {
	s.mu.Lock()
	target := s.lastAssistantEntryID
	s.mu.Unlock()
	if target == "" {
		return errors.New("no failed assistant entry to omit")
	}
	_, err := s.manager.AppendContextEdit(target, nil)
	return err
}

// rebuild swaps in a fresh agent built from durable state. Callers must already
// own the session slot (a running Prompt or a manual Compact).
func (s *AgentSession) rebuild() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrAgentSessionClosed
	}
	return s.rebuildAgentLocked()
}

// Messages returns a detached copy of the finalized session messages.
func (s *AgentSession) Messages() []agenttypes.AgentMessage {
	s.mu.Lock()
	manager := s.manager
	closed := s.closed
	s.mu.Unlock()
	if closed || manager == nil {
		return nil
	}
	return cloneAgentMessages(manager.BuildSessionContext().Messages)
}

// SetModel validates and applies a new model configuration between runs. An
// invalid model or a run in flight is rejected without changing state.
func (s *AgentSession) SetModel(options ModelOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrAgentSessionClosed
	}
	if s.running {
		return ErrAgentSessionBusy
	}
	if options.Model == nil {
		return errors.New("no model specified")
	}
	if options.StreamFn == nil {
		options.StreamFn = s.modelConfig.StreamFn
	}
	if options.APIKey == nil {
		options.APIKey = s.modelConfig.APIKey
	}
	resolved, err := ResolveModel(options)
	if err != nil {
		return err
	}
	previousModel := s.model
	previousConfig := s.modelConfig
	previousThinking := s.thinking

	s.model = resolved
	s.modelConfig = options
	if options.ThinkingLevel != "" {
		s.thinking = options.ThinkingLevel
	}
	if err := s.rebuildAgentLocked(); err != nil {
		s.model = previousModel
		s.modelConfig = previousConfig
		s.thinking = previousThinking
		return err
	}
	_, _ = s.manager.AppendModelChange(string(resolved.Provider), resolved.Id)
	return nil
}

// SetActiveTools changes the active registry entries between runs. Unknown
// names and concurrent runs are rejected.
func (s *AgentSession) SetActiveTools(names []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrAgentSessionClosed
	}
	if s.running {
		return ErrAgentSessionBusy
	}
	if err := s.registry.SetActive(names); err != nil {
		return err
	}
	return s.rebuildAgentLocked()
}

// Steer queues a steering message delivered after the current assistant turn.
func (s *AgentSession) Steer(text string) error {
	s.mu.Lock()
	closed := s.closed
	activeAgent := s.agent
	s.mu.Unlock()
	if closed || activeAgent == nil {
		return ErrAgentSessionClosed
	}
	activeAgent.Steer(userAgentMessage(text))
	return nil
}

// FollowUp queues a follow-up message delivered when the agent would stop.
func (s *AgentSession) FollowUp(text string) error {
	s.mu.Lock()
	closed := s.closed
	activeAgent := s.agent
	s.mu.Unlock()
	if closed || activeAgent == nil {
		return ErrAgentSessionClosed
	}
	activeAgent.FollowUp(userAgentMessage(text))
	return nil
}

// Abort requests that the active run stop, including an in-flight retry
// backoff or provider/tool call. It is safe when idle.
func (s *AgentSession) Abort() {
	s.mu.Lock()
	s.abortRequested = true
	activeAgent := s.agent
	cancel := s.runCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if activeAgent != nil {
		activeAgent.Abort()
	}
}

// Close unsubscribes every listener and releases the agent. A caller-owned
// manager is deliberately left open so the conversation can be reopened.
func (s *AgentSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	unsubscribe := s.unsubscribeAgent
	s.unsubscribeAgent = nil
	activeAgent := s.agent
	s.agent = nil
	s.listeners = nil
	manager := s.manager
	owns := s.ownsManager
	s.mu.Unlock()

	if activeAgent != nil {
		activeAgent.Abort()
	}
	if unsubscribe != nil {
		unsubscribe()
	}
	if owns && manager != nil {
		return manager.Close()
	}
	return nil
}

// Compact folds older messages into a caller-provided summary and rebuilds the
// session context. KeepRecentMessages bounds how much history stays verbatim,
// never splitting a tool-call/result pair. It fails when the session is busy or
// when no summarizer is configured, and a failed summarization leaves the
// original history untouched.
func (s *AgentSession) Compact(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrAgentSessionClosed
	}
	if s.running || s.compacting {
		s.mu.Unlock()
		return ErrAgentSessionBusy
	}
	s.compacting = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.compacting = false
		s.mu.Unlock()
	}()

	return s.compactInternal(ctx)
}

// compactInternal performs one compaction. The caller must own the session slot
// (Prompt or Compact). A failed summary is returned before any mutation, so the
// durable history stays intact.
func (s *AgentSession) compactInternal(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrAgentSessionClosed
	}
	summarize := s.policy.Summarize
	keep := s.policy.KeepRecentMessages
	manager := s.manager
	s.mu.Unlock()

	if summarize == nil {
		return errNoSummarizer
	}
	if keep <= 0 {
		keep = defaultKeepRecentMessages
	}

	entries := manager.BuildContextEntries()
	cut := selectCompactionCut(entries, keep)
	if cut < 0 {
		return nil
	}

	var older []agenttypes.AgentMessage
	var tokensBefore float64
	for index := 0; index < cut; index++ {
		for _, message := range SessionEntryToContextMessages(entries[index]) {
			if messageRoleOf(message) == aitypes.SystemMessageRole {
				continue
			}
			older = append(older, message)
			tokensBefore += float64(EstimateTokens(message))
		}
	}
	if len(older) == 0 {
		return nil
	}

	summary, err := summarize(ctx, older)
	if err != nil {
		return err
	}
	if strings.TrimSpace(summary) == "" {
		return errors.New("compaction summary is empty")
	}

	if _, err := manager.AppendCompaction(CompactionInput{
		Summary:          summary,
		FirstKeptEntryID: entries[cut].ID,
		TokensBefore:     tokensBefore,
	}); err != nil {
		return err
	}
	return s.rebuild()
}

// maybeCompact applies the optional threshold compaction before a prompt.
// CompactReserveTokens overrides the reserve for tests; otherwise the model's
// output capacity is reserved so a request never overflows the context window.
// The model's declared limits are not mutated.
func (s *AgentSession) maybeCompact(ctx context.Context) error {
	s.mu.Lock()
	reserve := s.policy.CompactReserveTokens
	summarize := s.policy.Summarize
	contextWindow := float64(0)
	maxTokens := float64(0)
	if s.model != nil {
		contextWindow = s.model.ContextWindow
		maxTokens = s.model.MaxTokens
	}
	closed := s.closed
	s.mu.Unlock()
	if closed || summarize == nil || contextWindow <= 0 {
		return nil
	}
	if reserve <= 0 {
		reserve = int(maxTokens)
	}
	if reserve <= 0 {
		return nil
	}
	messages := s.Messages()
	estimated := float64(0)
	for _, message := range messages {
		estimated += float64(EstimateTokens(message))
	}
	if estimated+float64(reserve) <= contextWindow {
		return nil
	}
	return s.compactInternal(ctx)
}

// Stats summarizes the durable transcript.
func (s *AgentSession) Stats() SessionStats {
	stats := SessionStats{}
	s.mu.Lock()
	manager := s.manager
	stats.SessionID = manager.SessionID()
	if file := manager.SessionFile(); file != "" {
		stats.SessionFile = &file
	}
	s.mu.Unlock()

	entries := manager.Entries()
	for _, entry := range entries {
		if entry.Type == "message" {
			stats.TotalMessages++
			var message aitypes.Message
			if err := json.Unmarshal(entry.Payload, &message); err != nil {
				continue
			}
			switch message.Role {
			case aitypes.UserMessageRole:
				stats.UserMessages++
			case aitypes.AssistantMessageRole:
				stats.AssistantMessages++
				if message.Assistant != nil {
					for _, block := range message.Assistant.Content {
						if block.IsToolCall() {
							stats.ToolCalls++
						}
					}
					usage := message.Assistant.Usage
					stats.InputTokens += usage.Input
					stats.OutputTokens += usage.Output
					stats.CacheRead += usage.CacheRead
					stats.CacheWrite += usage.CacheWrite
					stats.TotalTokens += usage.TotalTokens
					stats.Cost += usage.Cost.Total
				}
			case aitypes.ToolResultMessageRole:
				stats.ToolResults++
			}
		}
		key, usage := usageOfEntry(entry)
		if key == "Tools/summaries" && usage != nil {
			stats.InputTokens += usage.Input
			stats.OutputTokens += usage.Output
			stats.CacheRead += usage.CacheRead
			stats.CacheWrite += usage.CacheWrite
			stats.TotalTokens += usage.TotalTokens
			stats.Cost += usage.Cost.Total
		}
	}
	return stats
}

// CycleModel selects the next model in the caller-provided scope. The embedded
// session only owns one model, so the scope of a single model cycles to itself;
// callers that need provider cycling can call SetModel directly.
func (s *AgentSession) CycleModel() (ModelCycleResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ModelCycleResult{}, ErrAgentSessionClosed
	}
	if s.running {
		return ModelCycleResult{}, ErrAgentSessionBusy
	}
	return ModelCycleResult{Model: s.model, ThinkingLevel: s.thinking, IsScoped: false}, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// assistantMessageOf extracts an assistant message from an agent message.
func assistantMessageOf(message *agenttypes.AgentMessage) (aitypes.AssistantMessage, bool) {
	if message == nil || message.Message == nil || message.Message.Assistant == nil {
		return aitypes.AssistantMessage{}, false
	}
	return *message.Message.Assistant, true
}

// userAgentMessage builds a standard user message for steering/follow-up.
func userAgentMessage(text string) agenttypes.AgentMessage {
	user := aitypes.NewUserMessage(text, float64(time.Now().UnixMilli()))
	return agenttypes.NewAgentMessageFromMessage(aitypes.NewUserMessageVariant(user))
}

// cloneAgentMessage returns a JSON-round-tripped copy so callers never share
// mutable message state with the session.
func cloneAgentMessage(message agenttypes.AgentMessage) agenttypes.AgentMessage {
	raw, err := json.Marshal(message)
	if err != nil {
		return message
	}
	var clone agenttypes.AgentMessage
	if err := json.Unmarshal(raw, &clone); err != nil {
		return message
	}
	return clone
}

func cloneAgentMessages(messages []agenttypes.AgentMessage) []agenttypes.AgentMessage {
	if messages == nil {
		return nil
	}
	out := make([]agenttypes.AgentMessage, 0, len(messages))
	for _, message := range messages {
		out = append(out, cloneAgentMessage(message))
	}
	return out
}

// messageText returns the concatenated text content of a message.
func messageText(message agenttypes.AgentMessage) string {
	if message.Message == nil {
		return ""
	}
	var builder strings.Builder
	switch message.Message.Role {
	case aitypes.UserMessageRole:
		if message.Message.User != nil {
			content := message.Message.User.Content
			if content.Structured {
				for _, block := range content.Blocks {
					if block.Text != nil {
						builder.WriteString(block.Text.Text)
					}
				}
			} else {
				builder.WriteString(content.Text)
			}
		}
	case aitypes.AssistantMessageRole:
		if message.Message.Assistant != nil {
			for _, block := range message.Message.Assistant.Content {
				if block.Text != nil {
					builder.WriteString(block.Text.Text)
				}
			}
		}
	case aitypes.ToolResultMessageRole:
		if message.Message.ToolResult != nil {
			for _, block := range message.Message.ToolResult.Content {
				if block.Text != nil {
					builder.WriteString(block.Text.Text)
				}
			}
		}
	case aitypes.SystemMessageRole:
		if message.Message.System != nil {
			builder.WriteString(message.Message.System.Content.Text)
		}
	}
	return builder.String()
}
