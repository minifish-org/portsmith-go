// Self-tests for the embedded SDK resilience contract.
//
// These tests use offline fake provider stream functions and never call a paid
// model, spawn Node or require a Pi binary. They cover retry exhaustion and
// classification, real cancellation during a provider call and during retry
// backoff, length-truncated tool calls that must never execute, automatic and
// manual compaction, failed-summary rollback, cancellation followed by a fresh
// prompt, steering/follow-up queues, MaxTurns, reopen durability and clean
// Close.
//
// Upstream: Copyright (c) 2025 Mario Zechner, MIT License.
package codingagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agenttypes "github.com/minifish-org/pith/packages/agent/types"
	aitypes "github.com/minifish-org/pith/packages/ai/types"
)

// resilienceErrorStream builds an error response with an explicit message.
func resilienceErrorStream(message string) *aitypes.AssistantMessageEventStream {
	stream := aitypes.NewAssistantMessageEventStream()
	assistant := aitypes.NewAssistantMessage(aitypes.ApiOpenAICompletions, aitypes.ProviderOpenAI, "test-model", 1)
	assistant.StopReason = aitypes.StopReasonError
	assistant.ErrorMessage = &message
	stream.Push(aitypes.NewErrorEvent(aitypes.StopReasonError, assistant))
	return stream
}

func TestResilienceRetryExhaustionReturnsError(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	var calls int32
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		atomic.AddInt32(&calls, 1)
		return resilienceErrorStream("503 service unavailable")
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model:  ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{RetryAttempts: 2, RetryDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	result, err := session.Prompt(context.Background(), "start")
	if err == nil {
		t.Fatal("retry exhaustion did not return an error")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
	if result.StopReason != aitypes.StopReasonError {
		t.Fatalf("stop reason = %q", result.StopReason)
	}
	if len(result.Messages) == 0 {
		t.Fatal("failed run lost its persisted context")
	}
}

func TestResilienceNonRetryableAuthFailsFast(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	var calls int32
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		atomic.AddInt32(&calls, 1)
		return resilienceErrorStream("401 unauthorized: invalid api key")
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model:  ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{RetryAttempts: 5, RetryDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if _, err = session.Prompt(context.Background(), "start"); err == nil {
		t.Fatal("auth failure did not return an error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("auth failure retried %d times", got)
	}
}

func TestResilienceNegativeDisablesRetry(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	var calls int32
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		atomic.AddInt32(&calls, 1)
		return resilienceErrorStream("503 service unavailable")
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model:  ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{RetryAttempts: -1, RetryDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if _, err = session.Prompt(context.Background(), "start"); err == nil {
		t.Fatal("negative budget did not surface the error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("negative budget still retried: calls=%d", got)
	}
}

func TestResilienceZeroUsesDefaultBudget(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	var calls int32
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		atomic.AddInt32(&calls, 1)
		return resilienceErrorStream("503 service unavailable")
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model:  ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{RetryDelay: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if _, err = session.Prompt(context.Background(), "start"); err == nil {
		t.Fatal("default budget did not surface the error")
	}
	if got := atomic.LoadInt32(&calls); got != int32(defaultRetryAttempts+1) {
		t.Fatalf("default budget calls = %d, want %d", got, defaultRetryAttempts+1)
	}
}

func TestResilienceRetryEventsEmitted(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	secret := "sk-abcdefghijklmnopqrstuvwxyz012345"
	var calls int32
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		if atomic.AddInt32(&calls, 1) == 1 {
			return resilienceErrorStream("503 service unavailable authorization=Bearer " + secret)
		}
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("recovered"))
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model:  ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{RetryAttempts: 2, RetryDelay: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	events := make(chan string, 8)
	off := session.Subscribe(func(event SessionEvent) {
		if event.Type == SessionEventAutoRetryStart || event.Type == SessionEventAutoRetryEnd {
			events <- event.Text
		}
	})
	defer off()

	if _, err = session.Prompt(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	sawStart := false
	for !sawStart {
		select {
		case text := <-events:
			if strings.Contains(text, secret) {
				t.Fatalf("retry event leaked a credential: %q", text)
			}
			if strings.Contains(text, "attempt=1") {
				sawStart = true
			}
		case <-deadline:
			t.Fatal("retry start event missing")
		}
	}
}

func TestResilienceCancelDuringBackoff(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	var calls int32
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		atomic.AddInt32(&calls, 1)
		return resilienceErrorStream("503 service unavailable")
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model:  ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{RetryAttempts: 5, RetryDelay: 250 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	done := make(chan error, 1)
	go func() { _, promptErr := session.Prompt(context.Background(), "start"); done <- promptErr }()
	time.Sleep(50 * time.Millisecond)
	session.Abort()
	select {
	case promptErr := <-done:
		if promptErr == nil {
			t.Fatal("abort during backoff was swallowed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abort during backoff did not return")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls after backoff abort = %d, want 1", got)
	}
}

func TestResilienceCancelDuringProvider(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	entered := make(chan struct{})
	cancelled := make(chan struct{})
	stream := func(model *aitypes.Model, _ *aitypes.TranscriptContext, options *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		response := aitypes.NewAssistantMessageEventStream()
		close(entered)
		go func() {
			select {
			case <-options.Signal:
				close(cancelled)
				assistant := aitypes.NewAssistantMessage(model.Api, model.Provider, model.Id, 1)
				assistant.StopReason = aitypes.StopReasonAborted
				response.Push(aitypes.NewErrorEvent(aitypes.StopReasonAborted, assistant))
			case <-time.After(2 * time.Second):
				assistant := aitypes.NewAssistantMessage(model.Api, model.Provider, model.Id, 1)
				assistant.StopReason = aitypes.StopReasonStop
				response.End(&assistant)
			}
		}()
		return response
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model:  ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{RetryAttempts: 3, RetryDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	done := make(chan error, 1)
	go func() { _, promptErr := session.Prompt(context.Background(), "wait"); done <- promptErr }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider not started")
	}
	if _, err = session.Prompt(context.Background(), "concurrent"); err == nil {
		t.Fatal("concurrent Prompt accepted")
	}
	session.Abort()
	select {
	case promptErr := <-done:
		if promptErr == nil {
			t.Fatal("abort swallowed")
		}
	case <-time.After(time.Second):
		t.Fatal("abort did not return")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("provider signal not cancelled")
	}
}

func TestResilienceLengthToolCallNeverExecuted(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	var executions int32
	registry, err := NewToolRegistry(dir, []ToolDefinition{{
		Name:        "danger",
		Description: "must never run",
		Parameters:  json.RawMessage(`{"type":"object"}`),
		Execute: func(context.Context, json.RawMessage) (ToolResult, error) {
			atomic.AddInt32(&executions, 1)
			return ToolResult{}, nil
		},
	}}, []string{"danger"}, nil, ToolHooks{})
	if err != nil {
		t.Fatal(err)
	}

	var calls int32
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		if atomic.AddInt32(&calls, 1) == 1 {
			call := aitypes.NewToolCall("cut", "danger", json.RawMessage(`{}`))
			return sessionDone(aitypes.StopReasonLength, aitypes.ContentBlock{Type: aitypes.ContentTypeToolCall, ToolCall: &call})
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

	result, err := session.Prompt(context.Background(), "start")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != aitypes.StopReasonStop {
		t.Fatalf("stop reason = %q", result.StopReason)
	}
	if got := atomic.LoadInt32(&executions); got != 0 {
		t.Fatalf("length-truncated tool call executed %d times", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls = %d, want 2 (continuation after length)", got)
	}
}

func TestResilienceAutoAndManualCompaction(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	var summaries int32
	var requests int32
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		atomic.AddInt32(&requests, 1)
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("answer "+strings.Repeat("x", 300)))
	}
	small := sessionTestModel()
	small.ContextWindow = 256
	small.MaxTokens = 32
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model: ModelOptions{Model: small, StreamFn: stream},
		Policy: RunPolicy{
			CompactReserveTokens: 64,
			KeepRecentMessages:   2,
			Summarize: func(_ context.Context, messages []agenttypes.AgentMessage) (string, error) {
				if len(messages) == 0 {
					t.Error("empty summary input")
				}
				atomic.AddInt32(&summaries, 1)
				return "SUMMARY-SENTINEL", nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < 4; i++ {
		if _, err = session.Prompt(ctx, strings.Repeat("long ", 90)); err != nil {
			t.Fatal(err)
		}
	}
	if atomic.LoadInt32(&summaries) == 0 {
		t.Fatal("automatic compaction absent")
	}
	if err = session.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(session.Messages())
	if !strings.Contains(string(encoded), "SUMMARY-SENTINEL") {
		t.Fatal("summary not in transcript")
	}
}

func TestResilienceCompactionDoesNotSplitPairs(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	// user -> assistant(tool call) -> tool result -> assistant(text). A naive
	// "keep one message" cut would retain only the tool result and orphan it;
	// the pair-aware cut must move the boundary to the user turn start.
	appendMessage := func(message aitypes.Message) {
		raw, err := json.Marshal(agenttypes.NewAgentMessageFromMessage(message))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = manager.AppendMessage(raw); err != nil {
			t.Fatal(err)
		}
	}
	appendMessage(aitypes.NewUserMessageVariant(aitypes.NewUserMessage("do it", 1)))
	call := aitypes.NewToolCall("call-1", "danger", json.RawMessage(`{}`))
	assistant := aitypes.NewAssistantMessage(aitypes.ApiOpenAICompletions, aitypes.ProviderOpenAI, "test-model", 2)
	assistant.Content = []aitypes.ContentBlock{{Type: aitypes.ContentTypeToolCall, ToolCall: &call}}
	assistant.StopReason = aitypes.StopReasonToolUse
	appendMessage(aitypes.NewAssistantMessageVariant(assistant))
	appendMessage(aitypes.NewToolResultMessageVariant(aitypes.ToolResultMessage{
		ToolCallId: "call-1",
		ToolName:   "danger",
		Content:    []aitypes.ContentBlock{aitypes.TextBlock("result")},
	}))
	final := aitypes.NewAssistantMessage(aitypes.ApiOpenAICompletions, aitypes.ProviderOpenAI, "test-model", 3)
	final.Content = []aitypes.ContentBlock{aitypes.TextBlock("done")}
	final.StopReason = aitypes.StopReasonStop
	appendMessage(aitypes.NewAssistantMessageVariant(final))

	entries := manager.BuildContextEntries()
	cut := selectCompactionCut(entries, 1)
	if cut < 0 {
		t.Fatal("expected a safe cut")
	}
	messages := SessionEntryToContextMessages(entries[cut])
	if len(messages) == 0 || messageRoleOf(messages[0]) != aitypes.UserMessageRole {
		t.Fatalf("cut split a tool-call/result pair: %+v", messages)
	}
	_ = dir
}

func TestResilienceFailedSummaryRollback(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("keep history"))
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model: ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{
			KeepRecentMessages: 1,
			Summarize: func(context.Context, []agenttypes.AgentMessage) (string, error) {
				return "", context.Canceled
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	for i := 0; i < 3; i++ {
		if _, err = session.Prompt(context.Background(), "history"); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := json.Marshal(session.Messages())
	if err = session.Compact(context.Background()); err == nil {
		t.Fatal("summary failure was hidden")
	}
	after, _ := json.Marshal(session.Messages())
	if string(before) != string(after) {
		t.Fatal("failed compaction damaged history")
	}
}

func TestResilienceCancellationThenNextPrompt(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	var calls int32
	blocking := make(chan struct{})
	stream := func(model *aitypes.Model, _ *aitypes.TranscriptContext, options *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		response := aitypes.NewAssistantMessageEventStream()
		if atomic.AddInt32(&calls, 1) == 1 {
			go func() {
				<-blocking
				select {
				case <-options.Signal:
					assistant := aitypes.NewAssistantMessage(model.Api, model.Provider, model.Id, 1)
					assistant.StopReason = aitypes.StopReasonAborted
					response.Push(aitypes.NewErrorEvent(aitypes.StopReasonAborted, assistant))
				}
			}()
			return response
		}
		assistant := aitypes.NewAssistantMessage(model.Api, model.Provider, model.Id, 1)
		assistant.StopReason = aitypes.StopReasonStop
		assistant.Content = []aitypes.ContentBlock{aitypes.TextBlock("second")}
		response.Push(aitypes.NewDoneEvent(aitypes.StopReasonStop, assistant))
		return response
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model:  ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{RetryAttempts: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	done := make(chan error, 1)
	go func() { _, promptErr := session.Prompt(context.Background(), "first"); done <- promptErr }()
	time.Sleep(20 * time.Millisecond)
	session.Abort()
	close(blocking)
	select {
	case promptErr := <-done:
		if promptErr == nil {
			t.Fatal("cancelled prompt returned no error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled prompt did not return")
	}

	result, err := session.Prompt(context.Background(), "second")
	if err != nil {
		t.Fatalf("prompt after cancellation failed: %v", err)
	}
	if result.StopReason != aitypes.StopReasonStop {
		t.Fatalf("stop reason = %q", result.StopReason)
	}
}

func TestResilienceSteeringAndFollowUp(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	var session *AgentSession
	seenSteer := false
	seenFollow := false
	var calls int32
	stream := func(_ *aitypes.Model, transcript *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		count := atomic.AddInt32(&calls, 1)
		if count > 6 {
			t.Error("queues never drained")
			return sessionDone(aitypes.StopReasonError)
		}
		encoded, _ := json.Marshal(transcript)
		text := string(encoded)
		seenSteer = seenSteer || strings.Contains(text, "steering-sentinel")
		seenFollow = seenFollow || strings.Contains(text, "followup-sentinel")
		if count == 1 {
			if err := session.Steer("steering-sentinel"); err != nil {
				t.Error(err)
			}
			if err := session.FollowUp("followup-sentinel"); err != nil {
				t.Error(err)
			}
		}
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("done"))
	}
	var err error
	session, err = CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model: ModelOptions{Model: sessionTestModel(), StreamFn: stream},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if _, err = session.Prompt(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	if !seenSteer || !seenFollow {
		t.Fatal("queued messages not delivered before completion")
	}
}

func TestResilienceMaxTurns(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	registry, err := NewToolRegistry(dir, []ToolDefinition{
		sessionTool("looper", func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{Content: []aitypes.ContentBlock{aitypes.TextBlock("again")}}, nil
		}),
	}, []string{"looper"}, nil, ToolHooks{})
	if err != nil {
		t.Fatal(err)
	}

	var calls int32
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		atomic.AddInt32(&calls, 1)
		return sessionDone(aitypes.StopReasonToolUse, sessionToolCall("loop", "looper", `{}`))
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager, Tools: registry,
		Model:  ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{MaxTurns: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	result, err := session.Prompt(context.Background(), "start")
	if err == nil {
		t.Fatal("MaxTurns did not surface an error")
	}
	if result.Turns != 2 {
		t.Fatalf("turns = %d, want 2", result.Turns)
	}
	if len(session.Messages()) == 0 {
		t.Fatal("MaxTurns lost persisted context")
	}
}

func TestResilienceCompactionReopen(t *testing.T) {
	dir := t.TempDir()
	manager, err := OpenSession(dir + "/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("ok"))
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model: ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{
			KeepRecentMessages: 1,
			Summarize: func(context.Context, []agenttypes.AgentMessage) (string, error) {
				return "REOPEN-SENTINEL", nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = session.Prompt(context.Background(), "history"); err != nil {
			t.Fatal(err)
		}
	}
	if err = session.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model: ModelOptions{Model: sessionTestModel(), StreamFn: stream},
		Policy: RunPolicy{
			KeepRecentMessages: 1,
			Summarize: func(context.Context, []agenttypes.AgentMessage) (string, error) {
				return "REOPEN-SENTINEL", nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	encoded, _ := json.Marshal(restored.Messages())
	if !strings.Contains(string(encoded), "REOPEN-SENTINEL") {
		t.Fatal("compacted history was not restored on reopen")
	}
}

func TestResilienceCleanClose(t *testing.T) {
	manager, dir := sessionManager(t)
	defer manager.Close()

	stream := func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		return sessionDone(aitypes.StopReasonStop, aitypes.TextBlock("ok"))
	}
	session, err := CreateAgentSession(SessionOptions{
		Cwd: dir, Manager: manager,
		Model: ModelOptions{Model: sessionTestModel(), StreamFn: stream},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.Prompt(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	if err = session.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err = session.Prompt(context.Background(), "after"); err == nil {
		t.Fatal("Prompt after Close accepted")
	}
}
