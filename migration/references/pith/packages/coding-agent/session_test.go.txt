// Self-tests for the embedded SDK session.
//
// These tests exercise the public session contract with offline fake provider
// stream functions: they never call a paid model, spawn Node or require a Pi
// binary. They cover the tool loop, usage/turn accounting, tool failure
// recovery, unknown tools, model and tool changes, queue ordering, listener
// cleanup, cross-session isolation, restart after a completed turn,
// compaction, retry and close semantics.
//
// Upstream: Copyright (c) 2025 Mario Zechner, MIT License.
package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agenttypes "github.com/minifish-org/pith/packages/agent/types"
	aitypes "github.com/minifish-org/pith/packages/ai/types"
)

// ---------------------------------------------------------------------------
// fake provider helpers
// ---------------------------------------------------------------------------

func sessionDone(reason aitypes.StopReason, blocks ...aitypes.ContentBlock) *aitypes.AssistantMessageEventStream {
	stream := aitypes.NewAssistantMessageEventStream()
	message := aitypes.NewAssistantMessage(aitypes.ApiOpenAICompletions, aitypes.ProviderOpenAI, "test-model", 1)
	message.Content = blocks
	message.StopReason = reason
	message.Usage = aitypes.Usage{Input: 10, Output: 5, TotalTokens: 15}
	switch reason {
	case aitypes.StopReasonError:
		text := "503 transient provider failure"
		message.ErrorMessage = &text
		stream.Push(aitypes.NewErrorEvent(reason, message))
	default:
		stream.Push(aitypes.NewDoneEvent(reason, message))
	}
	return stream
}

func sessionToolCall(id, name string, arguments string) aitypes.ContentBlock {
	call := aitypes.NewToolCall(id, name, json.RawMessage(arguments))
	return aitypes.ContentBlock{Type: aitypes.ContentTypeToolCall, ToolCall: &call}
}

func transcriptJSON(context *aitypes.TranscriptContext) string {
	data, err := json.Marshal(context)
	if err != nil {
		return ""
	}
	return string(data)
}

func sessionTool(name string, execute func(context.Context, json.RawMessage) (ToolResult, error)) ToolDefinition {
	return ToolDefinition{
		Name:        name,
		Description: "session self-test tool",
		Parameters:  json.RawMessage(`{"type":"object"}`),
		Execute:     execute,
	}
}

// sessionManager opens a durable manager in a temp dir.
func sessionManager(t *testing.T) (*SessionManager, string) {
	t.Helper()
	dir := t.TempDir()
	manager, err := OpenSession(filepath.Join(dir, "session.jsonl"))
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	return manager, dir
}

// ---------------------------------------------------------------------------
// tool loop, usage, events and reopen
// ---------------------------------------------------------------------------

func TestAgentSessionPromptToolLoopEventsAndReopen(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	executions := 0
	registry, err := NewToolRegistry(dir, []ToolDefinition{
		sessionTool("session_probe", func(context.Context, json.RawMessage) (ToolResult, error) {
			executions++
			return ToolResult{Content: []aitypes.ContentBlock{aitypes.TextBlock("probe-output")}}, nil
		}),
	}, []string{"session_probe"}, nil, ToolHooks{})
	if err != nil {
		t.Fatal(err)
	}

	requests := 0
	stream := func(model *aitypes.Model, context *aitypes.TranscriptContext, options *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		requests++
		if model.ContextWindow != 1000000 || model.MaxTokens != 384000 {
			t.Errorf("model capacity lost: context=%v max=%v", model.ContextWindow, model.MaxTokens)
		}
		if requests == 1 {
			return sessionDone(aitypes.StopReasonToolUse, sessionToolCall("call-1", "session_probe", `{}`))
		}
		if !strings.Contains(transcriptJSON(context), "probe-output") {
			t.Errorf("tool result missing from follow-up request")
		}
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("done"))
	}

	session, err := CreateAgentSession(SessionOptions{
		Cwd:     dir,
		Manager: manager,
		Model:   ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Tools:   registry,
	})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var events []string
	off := session.Subscribe(func(event SessionEvent) {
		mu.Lock()
		events = append(events, event.Type)
		mu.Unlock()
	})

	result, err := session.Prompt(context.Background(), "fix code")
	if err != nil {
		t.Fatal(err)
	}
	if executions != 1 || requests != 2 {
		t.Fatalf("executions=%d requests=%d", executions, requests)
	}
	if result.StopReason != aitypes.StopReasonStop || result.Turns != 2 {
		t.Fatalf("result stop=%s turns=%d", result.StopReason, result.Turns)
	}
	if result.Usage.Input != 20 || result.Usage.Output != 10 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if len(result.Messages) < 4 {
		t.Fatalf("run messages = %d", len(result.Messages))
	}

	mu.Lock()
	joined := strings.Join(events, ",")
	mu.Unlock()
	start := strings.Index(joined, "tool_execution_start")
	end := strings.Index(joined, "tool_execution_end")
	if start < 0 || end <= start {
		t.Fatalf("tool events missing or out of order: %s", joined)
	}
	if !strings.Contains(joined, "turn_start") || !strings.Contains(joined, "agent_start") || !strings.Contains(joined, "agent_end") {
		t.Fatalf("lifecycle events missing: %s", joined)
	}

	// Snapshots must be detached from session state.
	result.Messages[0] = agenttypes.AgentMessage{}
	if len(session.Messages()) == 0 {
		t.Fatal("session messages lost")
	}

	off()
	if session.SubscriberCount() != 0 {
		t.Fatalf("listener not removed: %d", session.SubscriberCount())
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := CreateAgentSession(SessionOptions{
		Cwd:     dir,
		Manager: manager,
		Model:   ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Tools:   registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if len(restored.Messages()) < 4 {
		t.Fatalf("conversation lost on reopen: %d", len(restored.Messages()))
	}
}

// ---------------------------------------------------------------------------
// concurrency and close
// ---------------------------------------------------------------------------

func TestAgentSessionConcurrentPromptRejectedAndClose(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	started := make(chan struct{})
	release := make(chan struct{})
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		close(started)
		<-release
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("done"))
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd:     dir,
		Manager: manager,
		Model:   ModelOptions{Model: sessionTestModel(), StreamFn: stream},
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, promptErr := session.Prompt(context.Background(), "first")
		done <- promptErr
	}()
	<-started

	if _, err := session.Prompt(context.Background(), "second"); !errors.Is(err, ErrAgentSessionBusy) {
		t.Fatalf("concurrent prompt error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Prompt(context.Background(), "after close"); !errors.Is(err, ErrAgentSessionClosed) {
		t.Fatalf("closed prompt error = %v", err)
	}
	if err := session.Steer("x"); !errors.Is(err, ErrAgentSessionClosed) {
		t.Fatalf("closed steer error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// tool failure and unknown tools
// ---------------------------------------------------------------------------

func TestAgentSessionToolFailureRecovery(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	registry, err := NewToolRegistry(dir, []ToolDefinition{
		sessionTool("session_fail", func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{}, errors.New("tool exploded")
		}),
	}, []string{"session_fail"}, nil, ToolHooks{})
	if err != nil {
		t.Fatal(err)
	}

	requests := 0
	recovered := false
	stream := func(_ *aitypes.Model, context *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		requests++
		if requests == 1 {
			return sessionDone(aitypes.StopReasonToolUse, sessionToolCall("call-fail", "session_fail", `{}`))
		}
		recovered = strings.Contains(transcriptJSON(context), "tool exploded")
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("recovered"))
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd:     dir,
		Manager: manager,
		Model:   ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Tools:   registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	result, err := session.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if !recovered {
		t.Fatal("error tool result not fed back to the model")
	}
	if result.StopReason != aitypes.StopReasonStop || result.Turns != 2 {
		t.Fatalf("result = %+v", result)
	}
}

func TestAgentSessionUnknownToolRecovers(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	requests := 0
	stream := func(_ *aitypes.Model, context *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		requests++
		if requests == 1 {
			return sessionDone(aitypes.StopReasonToolUse, sessionToolCall("call-unknown", "does_not_exist", `{}`))
		}
		if !strings.Contains(transcriptJSON(context), "not found") {
			t.Errorf("unknown tool error missing from context")
		}
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("done"))
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd:     dir,
		Manager: manager,
		Model:   ModelOptions{Model: sessionTestModel(), StreamFn: stream},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	result, err := session.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != aitypes.StopReasonStop || result.Turns != 2 {
		t.Fatalf("result = %+v", result)
	}
}

// ---------------------------------------------------------------------------
// model and tool changes
// ---------------------------------------------------------------------------

func TestAgentSessionSetModelBetweenRuns(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	var seen []string
	stream := func(model *aitypes.Model, _ *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		seen = append(seen, model.Id)
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("ok"))
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd:     dir,
		Manager: manager,
		Model:   ModelOptions{Model: sessionTestModel(), StreamFn: stream},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if _, err := session.Prompt(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	next := sessionTestModel()
	next.Id = "second-model"
	if err := session.SetModel(ModelOptions{Model: next, StreamFn: stream}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Prompt(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "test-model" || seen[1] != "second-model" {
		t.Fatalf("models seen = %v", seen)
	}

	if err := session.SetModel(ModelOptions{}); err == nil {
		t.Fatal("invalid model accepted")
	}
}

func TestAgentSessionSetActiveTools(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	registry, err := NewToolRegistry(dir, []ToolDefinition{
		sessionTool("tool_alpha", func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{Content: []aitypes.ContentBlock{aitypes.TextBlock("alpha")}}, nil
		}),
		sessionTool("tool_beta", func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{Content: []aitypes.ContentBlock{aitypes.TextBlock("beta")}}, nil
		}),
	}, []string{"tool_alpha", "tool_beta"}, nil, ToolHooks{})
	if err != nil {
		t.Fatal(err)
	}

	var declared string
	stream := func(_ *aitypes.Model, context *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		declared = transcriptJSON(context)
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("ok"))
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd:     dir,
		Manager: manager,
		Model:   ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Tools:   registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if err := session.SetActiveTools([]string{"tool_beta"}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(declared, "tool_beta") {
		t.Fatalf("active tool not declared: %s", declared)
	}
	if strings.Contains(declared, "tool_alpha") {
		t.Fatalf("removed tool still declared: %s", declared)
	}
	if err := session.SetActiveTools([]string{"unknown_tool"}); err == nil {
		t.Fatal("unknown tool activation accepted")
	}
}

// ---------------------------------------------------------------------------
// queues
// ---------------------------------------------------------------------------

func TestAgentSessionSteeringQueueOrder(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	var session *AgentSession
	registry, err := NewToolRegistry(dir, []ToolDefinition{
		sessionTool("session_steer", func(context.Context, json.RawMessage) (ToolResult, error) {
			err := session.Steer("steered-message")
			return ToolResult{}, err
		}),
	}, []string{"session_steer"}, nil, ToolHooks{})
	if err != nil {
		t.Fatal(err)
	}

	requests := 0
	sawSteer := false
	stream := func(_ *aitypes.Model, context *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		requests++
		if requests == 1 {
			return sessionDone(aitypes.StopReasonToolUse, sessionToolCall("call-steer", "session_steer", `{}`))
		}
		if strings.Contains(transcriptJSON(context), "steered-message") {
			sawSteer = true
		}
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("ok"))
	}
	session, err = CreateAgentSession(SessionOptions{
		Cwd:     dir,
		Manager: manager,
		Model:   ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Tools:   registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if _, err := session.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if !sawSteer {
		t.Fatal("steered message not delivered to the next request")
	}
}

func TestAgentSessionFollowUpQueueOrder(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	var session *AgentSession
	registry, err := NewToolRegistry(dir, []ToolDefinition{
		sessionTool("session_follow", func(context.Context, json.RawMessage) (ToolResult, error) {
			err := session.FollowUp("follow-message")
			return ToolResult{}, err
		}),
	}, []string{"session_follow"}, nil, ToolHooks{})
	if err != nil {
		t.Fatal(err)
	}

	requests := 0
	sawFollow := false
	stream := func(_ *aitypes.Model, context *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		requests++
		if requests == 1 {
			return sessionDone(aitypes.StopReasonToolUse, sessionToolCall("call-follow", "session_follow", `{}`))
		}
		if strings.Contains(transcriptJSON(context), "follow-message") {
			sawFollow = true
		}
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("ok"))
	}
	session, err = CreateAgentSession(SessionOptions{
		Cwd:     dir,
		Manager: manager,
		Model:   ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Tools:   registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	result, err := session.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if !sawFollow {
		t.Fatal("follow-up message not delivered")
	}
	if result.Turns != 3 {
		t.Fatalf("follow-up turns = %d", result.Turns)
	}
}

// ---------------------------------------------------------------------------
// cross-session isolation
// ---------------------------------------------------------------------------

func TestAgentSessionIndependentSessions(t *testing.T) {
	managerA, dirA := sessionManager(t)
	defer managerA.Close()
	managerB, dirB := sessionManager(t)
	defer managerB.Close()

	var countA, countB int32
	registryA, err := NewToolRegistry(dirA, []ToolDefinition{
		sessionTool("session_a", func(context.Context, json.RawMessage) (ToolResult, error) {
			atomic.AddInt32(&countA, 1)
			return ToolResult{Content: []aitypes.ContentBlock{aitypes.TextBlock("output-a")}}, nil
		}),
	}, []string{"session_a"}, nil, ToolHooks{})
	if err != nil {
		t.Fatal(err)
	}
	registryB, err := NewToolRegistry(dirB, []ToolDefinition{
		sessionTool("session_b", func(context.Context, json.RawMessage) (ToolResult, error) {
			atomic.AddInt32(&countB, 1)
			return ToolResult{Content: []aitypes.ContentBlock{aitypes.TextBlock("output-b")}}, nil
		}),
	}, []string{"session_b"}, nil, ToolHooks{})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	seenBySession := map[string][]string{}
	makeStream := func(tag, toolName string) agenttypes.StreamFn {
		requests := 0
		return func(_ *aitypes.Model, context *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
			requests++
			text := transcriptJSON(context)
			if requests == 1 {
				return sessionDone(aitypes.StopReasonToolUse, sessionToolCall("call-"+tag, toolName, `{}`))
			}
			mu.Lock()
			seenBySession[tag] = append(seenBySession[tag], text)
			mu.Unlock()
			return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("done-"+tag))
		}
	}

	sessionA, err := CreateAgentSession(SessionOptions{
		Cwd: dirA, Manager: managerA, Tools: registryA,
		Model: ModelOptions{Model: sessionTestModel(), StreamFn: makeStream("a", "session_a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sessionA.Close()
	sessionB, err := CreateAgentSession(SessionOptions{
		Cwd: dirB, Manager: managerB, Tools: registryB,
		Model: ModelOptions{Model: sessionTestModel(), StreamFn: makeStream("b", "session_b")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sessionB.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, promptErr := sessionA.Prompt(context.Background(), "a"); promptErr != nil {
			t.Errorf("session A: %v", promptErr)
		}
	}()
	go func() {
		defer wg.Done()
		if _, promptErr := sessionB.Prompt(context.Background(), "b"); promptErr != nil {
			t.Errorf("session B: %v", promptErr)
		}
	}()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for _, text := range seenBySession["a"] {
		if strings.Contains(text, "output-b") || strings.Contains(text, "session_b") {
			t.Fatal("session A saw session B state")
		}
	}
	for _, text := range seenBySession["b"] {
		if strings.Contains(text, "output-a") || strings.Contains(text, "session_a") {
			t.Fatal("session B saw session A state")
		}
	}
	if atomic.LoadInt32(&countA) != 1 || atomic.LoadInt32(&countB) != 1 {
		t.Fatalf("counts A=%d B=%d", countA, countB)
	}
}

// ---------------------------------------------------------------------------
// retry, compaction and stats
// ---------------------------------------------------------------------------

func TestAgentSessionRetriesTransientError(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	requests := 0
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		requests++
		if requests == 1 {
			return sessionDone(aitypes.StopReasonError)
		}
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("recovered"))
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd:     dir,
		Manager: manager,
		Model:   ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy:  RunPolicy{RetryAttempts: 1, RetryDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	result, err := session.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || result.StopReason != aitypes.StopReasonStop {
		t.Fatalf("requests=%d result=%+v", requests, result)
	}
}

func TestAgentSessionCompact(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	turn := 0
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		turn++
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("response"))
	}
	summarized := 0
	session, err := CreateAgentSession(SessionOptions{
		Cwd:     dir,
		Manager: manager,
		Model:   ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{
			KeepRecentMessages: 1,
			Summarize: func(_ context.Context, messages []agenttypes.AgentMessage) (string, error) {
				summarized++
				return "SUMMARY", nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	for i := 0; i < 3; i++ {
		if _, err := session.Prompt(context.Background(), "prompt"); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if summarized == 0 {
		t.Fatal("summarizer not invoked")
	}
	messages := session.Messages()
	if len(messages) == 0 {
		t.Fatal("compacted session lost all messages")
	}
	found := false
	for _, message := range messages {
		if message.Custom != nil && strings.Contains(string(message.Custom.Raw), "SUMMARY") {
			found = true
		}
	}
	if !found {
		t.Fatal("compaction summary missing from context")
	}

	noSummarizer, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model: ModelOptions{Model: sessionTestModel(), StreamFn: stream},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer noSummarizer.Close()
	if err := noSummarizer.Compact(context.Background()); err == nil {
		t.Fatal("compact without summarizer accepted")
	}
}

func TestAgentSessionStats(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	registry, err := NewToolRegistry(dir, []ToolDefinition{
		sessionTool("session_stats", func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{Content: []aitypes.ContentBlock{aitypes.TextBlock("ok")}}, nil
		}),
	}, []string{"session_stats"}, nil, ToolHooks{})
	if err != nil {
		t.Fatal(err)
	}

	requests := 0
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		requests++
		if requests == 1 {
			return sessionDone(aitypes.StopReasonToolUse, sessionToolCall("call-stats", "session_stats", `{}`))
		}
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("done"))
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager, Tools: registry,
		Model: ModelOptions{Model: sessionTestModel(), StreamFn: stream},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if _, err := session.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	stats := session.Stats()
	if stats.UserMessages != 1 || stats.AssistantMessages != 2 || stats.ToolCalls != 1 || stats.ToolResults != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.InputTokens != 20 || stats.OutputTokens != 10 {
		t.Fatalf("usage stats = %+v", stats)
	}
}

// ---------------------------------------------------------------------------
// small unit coverage for helper exports
// ---------------------------------------------------------------------------

func TestSessionEventHelpers(t *testing.T) {
	parsed := ParseSkillBlock("<skill name=\"demo\" location=\"/tmp/demo\">\nbody\n</skill>\n\nask me")
	if parsed == nil || parsed.Name != "demo" || parsed.Content != "body" || parsed.UserMessage != "ask me" {
		t.Fatalf("parse skill = %+v", parsed)
	}
	if ParseSkillBlock("plain") != nil {
		t.Fatal("plain text parsed as skill")
	}

	bus := CreateEventBus()
	var received []string
	off := bus.On("chan", func(data any) {
		received = append(received, data.(string))
	})
	bus.Emit("chan", "one")
	off()
	bus.Emit("chan", "two")
	if len(received) != 1 || received[0] != "one" {
		t.Fatalf("event bus = %v", received)
	}

	totals := CreateUsageTotals()
	AddUsageToTotals(&totals, aitypes.Usage{Input: 3, Output: 4, Cost: aitypes.UsageCost{Total: 1.5}})
	if totals.Input != 3 || totals.Output != 4 || totals.Cost != 1.5 {
		t.Fatalf("totals = %+v", totals)
	}

	env := "yes"
	manager, dir := sessionManager(t)
	defer manager.Close()
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model: ModelOptions{Model: sessionTestModel(), StreamFn: func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
			return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("ok"))
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if !IsInstallTelemetryEnabled(nil, &env) {
		t.Fatal("truthy telemetry env not honored")
	}
	if IsInstallTelemetryEnabled(nil, nil) {
		t.Fatal("nil settings and env should be disabled")
	}
}

// sessionTestModel returns an independent high-capacity model. It mirrors the
// judge model so a lost override is caught.
func sessionTestModel() *aitypes.Model {
	return &aitypes.Model{
		Id:            "test-model",
		Name:          "test-model",
		Api:           aitypes.ApiOpenAICompletions,
		Provider:      aitypes.ProviderOpenAI,
		BaseUrl:       "http://localhost.invalid/v1",
		ContextWindow: 1000000,
		MaxTokens:     384000,
	}
}
