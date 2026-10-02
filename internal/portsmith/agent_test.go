// agent_test.go adds candidate-owned coverage for the Pith coding-agent tool
// loop, durable sessions and model resilience. The independent judge exercises
// the main repair/resume path; these tests cover the additional source-owned
// behavior: validated legacy imports, model compatibility, unlimited versus
// explicit budgets, credential hygiene and the readonly system instructions.
package portsmith

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agenttypes "github.com/minifish-org/pith/packages/agent/types"
	aitypes "github.com/minifish-org/pith/packages/ai/types"
	codingagent "github.com/minifish-org/pith/packages/coding-agent"
)

const agentGoodCode = "package port\nfunc Value() int { return 42 }\n"
const agentBadCode = "package port\nfunc Value() int { return 41 }\n"
const agentSelfTest = "package port\nimport \"testing\"\nfunc TestCandidate(t *testing.T){if Value()<0{t.Fatal(\"negative\")}}\n"
const agentJudgeGo = "package port\nimport \"testing\"\nfunc TestPortsmithJudgeValue(t *testing.T){if Value()!=42{t.Fatal(\"wrong\")}}\n"

// agentFixture prepares a task whose independent judge requires Value()==42.
func agentFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	testPut(t, source, "LICENSE", "fixture license\n")
	testPut(t, source, "value.ts", "export function Value(){ return 42; }\n")
	judge := filepath.Join(dir, "judge")
	testPut(t, judge, "value_judge_test.go", agentJudgeGo)
	mod := filepath.Join(dir, "go.mod")
	testPut(t, dir, "go.mod", "module example.com/candidate\n\ngo 1.24\n")
	root, err := PrepareTask(context.Background(), PrepareOptions{
		Source:             source,
		Out:                filepath.Join(dir, "task"),
		Files:              []string{"value.ts"},
		Revision:           "fixture",
		Goal:               "Port Value returning 42",
		GoMod:              mod,
		Judge:              judge,
		RequiredJudgeTests: []string{"TestPortsmithJudgeValue"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func agentWriteCandidate(t *testing.T, root, code string) {
	t.Helper()
	if err := WriteCandidate(root, "value.go", []byte(code)); err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidate(root, "value_test.go", []byte(agentSelfTest)); err != nil {
		t.Fatal(err)
	}
}

func agentTestModel() ModelConfig {
	return ModelConfig{
		ID:            "deepseek-flash",
		BaseURL:       "http://127.0.0.1:1/v1",
		APIKey:        "fixture-secret-do-not-log",
		ContextWindow: 1000000,
		MaxTokens:     384000,
		Thinking:      "high",
	}
}

func agentResponse(reason aitypes.StopReason, blocks ...aitypes.ContentBlock) *aitypes.AssistantMessageEventStream {
	stream := aitypes.NewAssistantMessageEventStream()
	message := aitypes.NewAssistantMessage(aitypes.ApiOpenAICompletions, aitypes.ProviderOpenAI, "fixture", 1)
	message.Content = blocks
	message.StopReason = reason
	message.Usage = aitypes.Usage{Input: 10, Output: 5, TotalTokens: 15}
	stream.Push(aitypes.NewDoneEvent(reason, message))
	return stream
}

func agentToolCall(id, name string, args any) aitypes.ContentBlock {
	data, _ := json.Marshal(args)
	call := aitypes.NewToolCall(id, name, data)
	return aitypes.ContentBlock{Type: aitypes.ContentTypeToolCall, ToolCall: &call}
}

func agentContextText(t *testing.T, ctx *aitypes.TranscriptContext) string {
	t.Helper()
	data, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestRunPortOfflineRepairLoop drives a full bad-then-good repair through the
// Pith session with an offline stream and checks the durable session, the
// native tool set and the verify_candidate diagnostics.
func TestRunPortOfflineRepairLoop(t *testing.T) {
	root := agentFixture(t)
	agentWriteCandidate(t, root, agentBadCode)
	var calls atomic.Int32
	var sawBad, sawGood, sawTools atomic.Bool
	options := RunOptions{
		Root:     root,
		Model:    agentTestModel(),
		AgentDir: t.TempDir(),
		MaxTurns: 8,
		StreamFn: func(_ *aitypes.Model, transcript *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
			text := agentContextText(t, transcript)
			switch calls.Add(1) {
			case 1:
				sawTools.Store(true)
				for _, tool := range []string{"read", "write", "edit", "bash", "grep", "find", "ls", "verify_candidate"} {
					if !strings.Contains(text, `"name":"`+tool+`"`) {
						t.Errorf("native tool %s missing from the Pith request", tool)
					}
				}
				return agentResponse(aitypes.StopReasonToolUse, agentToolCall("verify-bad", "verify_candidate", map[string]any{}))
			case 2:
				sawBad.Store(strings.Contains(text, "behavior_failed"))
				return agentResponse(aitypes.StopReasonToolUse, agentToolCall("repair", "write", map[string]any{"path": "value.go", "content": agentGoodCode}))
			case 3:
				return agentResponse(aitypes.StopReasonToolUse, agentToolCall("verify-good", "verify_candidate", map[string]any{}))
			default:
				sawGood.Store(strings.Contains(text, "behavior_verified"))
				return agentResponse(aitypes.StopReasonStop, aitypes.TextBlock("candidate ready"))
			}
		},
	}
	report, err := RunPort(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "candidate_ready" || report.Turns != 4 {
		t.Fatalf("unexpected report %+v calls=%d", report, calls.Load())
	}
	if !sawTools.Load() || !sawBad.Load() || !sawGood.Load() {
		t.Fatalf("repair loop incomplete tools=%v bad=%v good=%v", sawTools.Load(), sawBad.Load(), sawGood.Load())
	}
	if report.Resumed {
		t.Fatal("a fresh task must not claim a resumed session")
	}
	if report.SessionFile == "" {
		t.Fatal("durable session path missing")
	}
	if _, err := os.Stat(report.SessionFile); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(report.SessionFile); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(data), "verify-bad") {
		t.Fatal("session did not persist the conversation")
	}
	if strings.Contains(string(agentReadFile(t, report.SessionFile)), options.Model.APIKey) {
		t.Fatal("credential persisted in the session file")
	}
}

// TestRunPortResumesDurableConversation ensures a second run reopens the same
// Pith session file and carries the prior conversation and feedback forward.
func TestRunPortResumesDurableConversation(t *testing.T) {
	root := agentFixture(t)
	agentWriteCandidate(t, root, agentBadCode)
	options := RunOptions{
		Root:     root,
		Model:    agentTestModel(),
		AgentDir: t.TempDir(),
		Feedback: "first-run-marker",
		StreamFn: func(_ *aitypes.Model, _ *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
			return agentResponse(aitypes.StopReasonStop, aitypes.TextBlock("done"))
		},
	}
	first, err := RunPort(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	var restored atomic.Bool
	options.Feedback = "second-run-marker"
	options.StreamFn = func(_ *aitypes.Model, transcript *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		text := agentContextText(t, transcript)
		restored.Store(strings.Contains(text, "first-run-marker") && strings.Contains(text, "second-run-marker"))
		return agentResponse(aitypes.StopReasonStop, aitypes.TextBlock("resumed"))
	}
	second, err := RunPort(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Resumed || second.SessionFile != first.SessionFile || !restored.Load() {
		t.Fatalf("conversation not resumed first=%+v second=%+v restored=%v", first, second, restored.Load())
	}
}

// A long resumed conversation uses a separate summary request before normal
// generation. Both requests must use the current run's explicit credentials,
// including after an operator rotates the key between runs.
func TestRunPortCompactionUsesCompatibleCredentials(t *testing.T) {
	root := agentFixture(t)
	agentWriteCandidate(t, root, agentGoodCode)
	manager, err := openRunSession(filepath.Join(root, "pith-sessions"), filepath.Join(root, "candidate"))
	if err != nil {
		t.Fatal(err)
	}
	sessionFile := manager.SessionFile()
	for _, text := range []string{strings.Repeat("older-context ", 4000), "recent-context"} {
		user := aitypes.NewUserMessageBlocks([]aitypes.ContentBlock{aitypes.TextBlock(text)}, 1)
		assistant := aitypes.NewAssistantMessage(aitypes.ApiOpenAICompletions, "portsmith-compatible", "local-model", 2)
		assistant.Content = []aitypes.ContentBlock{aitypes.TextBlock("previous-response")}
		assistant.StopReason = aitypes.StopReasonStop
		for _, message := range []aitypes.Message{aitypes.NewUserMessageVariant(user), aitypes.NewAssistantMessageVariant(assistant)} {
			data, err := json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.AppendMessage(data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	sessionFile, err = filepath.EvalSymlinks(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	var requests, summaries, generations atomic.Int32
	key := "rotated-fixture-secret-do-not-log"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("compatible request did not receive the current run credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		content := "candidate ready"
		tools, _ := body["tools"].([]any)
		if len(tools) == 0 {
			summaries.Add(1)
			content = "compacted-checkpoint-marker"
			if body["max_tokens"] != float64(codingagent.DefaultCompactionPolicy.ReserveTokens*8/10) {
				t.Error("summary output budget was lost", body["max_tokens"])
			}
		} else {
			generations.Add(1)
			data, _ := json.Marshal(body["messages"])
			if !strings.Contains(string(data), "compacted-checkpoint-marker") || strings.Contains(string(data), "older-context") {
				t.Error("generation did not continue from the compacted checkpoint")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		delta, _ := json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": content}, "finish_reason": nil}}})
		fmt.Fprintf(w, "data: %s\n\n", delta)
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model := ModelConfig{ID: "local-model", BaseURL: server.URL + "/v1", APIKey: key, ContextWindow: 100000, MaxTokens: 90000}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := RunPort(ctx, RunOptions{Root: root, Model: model, AgentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "candidate_ready" || !report.Resumed || report.SessionFile != sessionFile || requests.Load() != 2 || summaries.Load() != 1 || generations.Load() != 1 {
		t.Fatalf("compaction and resume failed: report=%+v requests=%d summaries=%d generations=%d", report, requests.Load(), summaries.Load(), generations.Load())
	}
	for _, name := range []string{sessionFile, filepath.Join(root, "last-run.json")} {
		if strings.Contains(agentReadString(t, name), key) {
			t.Fatal("compatible credential persisted in diagnostics or session")
		}
	}
}

// TestRunPortCancellationDoesNotCallProvider binds context cancellation to the
// session and never issues a provider request for a cancelled run.
func TestRunPortCancellationDoesNotCallProvider(t *testing.T) {
	root := agentFixture(t)
	agentWriteCandidate(t, root, agentGoodCode)
	var calls atomic.Int32
	options := RunOptions{
		Root:     root,
		Model:    agentTestModel(),
		AgentDir: t.TempDir(),
		StreamFn: func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
			calls.Add(1)
			return agentResponse(aitypes.StopReasonStop, aitypes.TextBlock("unexpected"))
		},
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	report, err := RunPort(cancelled, options)
	if err == nil {
		t.Fatal("cancelled run did not return an error")
	}
	if report.Status == "candidate_ready" {
		t.Fatal("cancelled run reported success")
	}
	if calls.Load() != 0 {
		t.Fatal("provider called after cancellation")
	}
}

// TestRunPortExplicitAndUnlimitedTurnBudgets checks that zero keeps the run
// unlimited while a positive budget stops a never-ending tool loop without
// reporting success.
func TestRunPortExplicitAndUnlimitedTurnBudgets(t *testing.T) {
	root := agentFixture(t)
	agentWriteCandidate(t, root, agentGoodCode)
	var calls atomic.Int32
	options := RunOptions{
		Root:     root,
		Model:    agentTestModel(),
		AgentDir: t.TempDir(),
		MaxTurns: 2,
		StreamFn: func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
			calls.Add(1)
			return agentResponse(aitypes.StopReasonToolUse, agentToolCall("read-loop", "read", map[string]any{"path": "value.go"}))
		},
	}
	report, err := RunPort(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status == "candidate_ready" {
		t.Fatalf("explicit turn budget ignored: %+v", report)
	}
	if calls.Load() != 2 {
		t.Fatalf("turn budget not enforced: %d calls", calls.Load())
	}

	// A zero budget continues until the model stops on its own.
	calls.Store(0)
	options.MaxTurns = 0
	options.StreamFn = func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		if calls.Add(1) < 4 {
			return agentResponse(aitypes.StopReasonToolUse, agentToolCall("read-loop", "ls", map[string]any{"path": "."}))
		}
		return agentResponse(aitypes.StopReasonStop, aitypes.TextBlock("finished"))
	}
	report, err = RunPort(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "candidate_ready" || calls.Load() != 4 {
		t.Fatalf("unlimited budget did not finish: %+v calls=%d", report, calls.Load())
	}
}

// TestRunPortLengthToolCallsAreNotExecuted verifies Pith's partial-tool
// rejection: a length-truncated write is never applied.
func TestRunPortLengthToolCallsAreNotExecuted(t *testing.T) {
	root := agentFixture(t)
	agentWriteCandidate(t, root, agentGoodCode)
	var calls atomic.Int32
	options := RunOptions{
		Root:     root,
		Model:    agentTestModel(),
		AgentDir: t.TempDir(),
		MaxTurns: 5,
		StreamFn: func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
			if calls.Add(1) == 1 {
				return agentResponse(aitypes.StopReasonLength, agentToolCall("truncated", "write", map[string]any{"path": "must-not-exist.go", "content": "package port"}))
			}
			return agentResponse(aitypes.StopReasonStop, aitypes.TextBlock("done"))
		},
	}
	report, err := RunPort(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "candidate_ready" || calls.Load() != 2 {
		t.Fatalf("length response not continued: %+v calls=%d", report, calls.Load())
	}
	if _, err := os.Stat(filepath.Join(root, "candidate", "must-not-exist.go")); !os.IsNotExist(err) {
		t.Fatal("length-truncated tool call was executed")
	}
}

// TestBuildRunModelPreservesCapacityAndCompat checks the catalog compatibility
// and declared capacities used for DeepSeek thinking.
func TestBuildRunModelPreservesCapacityAndCompat(t *testing.T) {
	model, deepseek, err := buildRunModel(agentTestModel())
	if err != nil {
		t.Fatal(err)
	}
	if !deepseek {
		t.Fatal("deepseek model not detected")
	}
	if model.ContextWindow != 1000000 || model.MaxTokens != 384000 {
		t.Fatalf("capacity lost: %+v", model)
	}
	if model.Api != aitypes.ApiOpenAICompletions || model.Provider != "portsmith-compatible" {
		t.Fatalf("unexpected provider: %s %s", model.Api, model.Provider)
	}
	if model.Compat.OpenAICompletions == nil || model.Compat.OpenAICompletions.ThinkingFormat == nil || *model.Compat.OpenAICompletions.ThinkingFormat != "deepseek" {
		t.Fatalf("DeepSeek thinking compatibility lost: %+v", model.Compat.OpenAICompletions)
	}
	if model.Compat.OpenAICompletions.MaxTokensField == nil || *model.Compat.OpenAICompletions.MaxTokensField != "max_tokens" {
		t.Fatal("max_tokens compatibility lost")
	}
	if model.Reasoning != true {
		t.Fatal("reasoning capability lost")
	}

	generic, deepseek, err := buildRunModel(ModelConfig{ID: "local-model", BaseURL: "http://localhost:8080/v1", ContextWindow: 32768, MaxTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if deepseek || generic.Compat.OpenAICompletions.ThinkingFormat != nil {
		t.Fatal("generic model claimed DeepSeek compatibility")
	}
	if generic.ContextWindow != 32768 || generic.MaxTokens != 4096 {
		t.Fatalf("generic capacity lost: %+v", generic)
	}
}

// TestConvertLegacyMessageValidatesRoles ensures only supported roles are
// imported and that Pi JSONL is converted rather than treated as Pith JSONL.
func TestConvertLegacyMessageValidatesRoles(t *testing.T) {
	user, ok := convertLegacyMessage(json.RawMessage(`{"role":"user","content":[{"type":"text","text":"legacy-user-marker"}],"timestamp":1}`))
	if !ok || !strings.Contains(string(user), "legacy-user-marker") {
		t.Fatalf("user message not converted: %s", user)
	}
	assistant, ok := convertLegacyMessage(json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"legacy-assistant-marker"}],"api":"openai-completions","provider":"openai","model":"fixture","timestamp":2}`))
	if !ok || !strings.Contains(string(assistant), "legacy-assistant-marker") {
		t.Fatalf("assistant message not converted: %s", assistant)
	}
	result, ok := convertLegacyMessage(json.RawMessage(`{"role":"toolResult","toolCallId":"call-1","toolName":"read","content":[{"type":"text","text":"legacy-tool-marker"}],"isError":false,"timestamp":3}`))
	if !ok || !strings.Contains(string(result), "legacy-tool-marker") {
		t.Fatalf("tool result not converted: %s", result)
	}
	if converted, ok := convertLegacyMessage(json.RawMessage(`{"role":"system","content":"not-imported"}`)); ok {
		t.Fatalf("unsupported role was imported: %s", converted)
	}
}

// TestRunImportLegacySkipsMalformedAndUnsupported exercises the full journal
// import, including a malformed line and an unsupported role.
func TestRunImportLegacySkipsMalformedAndUnsupported(t *testing.T) {
	root := t.TempDir()
	journal := "" +
		"{\"type\":\"message_end\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"legacy-user-marker\"}],\"timestamp\":1}}\n" +
		"not json at all\n" +
		"{\"type\":\"message_end\",\"message\":{\"role\":\"system\",\"content\":\"must-not-import\"}}\n" +
		"{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"ignored-event\"}]}}\n"
	if err := os.WriteFile(filepath.Join(root, "run-1000.jsonl"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := codingagent.OpenSession("")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := runImportLegacy(root, manager); err != nil {
		t.Fatal(err)
	}
	messages := manager.BuildSessionContext().Messages
	if len(messages) != 1 {
		t.Fatalf("expected one imported message, got %d", len(messages))
	}
	data, _ := json.Marshal(messages[0])
	if !strings.Contains(string(data), "legacy-user-marker") {
		t.Fatalf("expected legacy user marker, got %s", data)
	}
}

// TestRunSystemInstructionsPreserveBoundaries checks the migration instructions
// cover the readonly boundaries and the Go-only extension adaptation.
func TestRunSystemInstructionsPreserveBoundaries(t *testing.T) {
	text := strings.Join(runSystemInstructions(), "\n")
	for _, needle := range []string{"../references", "../judge", "read-only", "verify_candidate", "JavaScript extensions"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("system instructions missing %q", needle)
		}
	}
}

// TestRunPortModelErrorReportsUnknownCost ensures a terminal provider failure
// is reported without credentials and that cost is marked unknown.
func TestRunPortModelErrorReportsUnknownCost(t *testing.T) {
	root := agentFixture(t)
	agentWriteCandidate(t, root, agentGoodCode)
	options := RunOptions{
		Root:     root,
		Model:    agentTestModel(),
		AgentDir: t.TempDir(),
		StreamFn: func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
			err := "provider denied"
			message := aitypes.NewAssistantMessage(aitypes.ApiOpenAICompletions, aitypes.ProviderOpenAI, "fixture", 1)
			message.StopReason = aitypes.StopReasonError
			message.ErrorMessage = &err
			stream := aitypes.NewAssistantMessageEventStream()
			stream.Push(aitypes.NewErrorEvent(aitypes.StopReasonError, message))
			return stream
		},
	}
	report, err := RunPort(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "model_error" {
		t.Fatalf("provider failure not reported: %+v", report)
	}
	if strings.Contains(report.Error, options.Model.APIKey) {
		t.Fatal("credential leaked into the report")
	}
	if data := agentReadString(t, filepath.Join(root, "last-run.json")); strings.Contains(data, options.Model.APIKey) {
		t.Fatal("credential leaked into last-run.json")
	}
}

func agentReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func agentReadString(t *testing.T, path string) string {
	t.Helper()
	return string(agentReadFile(t, path))
}

// TestRunNativeStreamResolution checks the native provider fallback is present
// for the OpenAI-completions API and absent for unknown APIs.
func TestRunNativeStreamResolution(t *testing.T) {
	model, _, err := buildRunModel(agentTestModel())
	if err != nil {
		t.Fatal(err)
	}
	if runNativeStreamFn(model) == nil {
		t.Fatal("native stream function missing for openai-completions")
	}
	other := *model
	other.Api = "no-such-api"
	if runNativeStreamFn(&other) != nil {
		t.Fatal("native stream resolved for an unknown API")
	}
}

// TestRunThinkingLevelFallback checks invalid levels fall back to the SDK
// default instead of producing an invalid request.
func TestRunThinkingLevelFallback(t *testing.T) {
	if got := runThinkingLevel("high"); got != agenttypes.ThinkingHigh {
		t.Fatalf("valid level not preserved: %s", got)
	}
	if got := runThinkingLevel("not-a-level"); got != codingagent.DefaultThinkingLevel {
		t.Fatalf("invalid level did not fall back: %s", got)
	}
	if got := runThinkingLevel(""); got != codingagent.DefaultThinkingLevel {
		t.Fatalf("empty level did not fall back: %s", got)
	}
}

// TestRunPortTimeoutPreservesSession checks an explicit timeout produces a
// limited report and keeps the durable session on disk.
func TestRunPortTimeoutPreservesSession(t *testing.T) {
	root := agentFixture(t)
	agentWriteCandidate(t, root, agentGoodCode)
	options := RunOptions{
		Root:     root,
		Model:    agentTestModel(),
		AgentDir: t.TempDir(),
		Timeout:  40 * time.Millisecond,
		StreamFn: func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
			time.Sleep(200 * time.Millisecond)
			return agentResponse(aitypes.StopReasonStop, aitypes.TextBlock("late"))
		},
	}
	report, err := RunPort(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "timeout" {
		t.Fatalf("timeout not reported: %+v", report)
	}
	if report.SessionFile == "" {
		t.Fatal("timeout did not preserve the durable session")
	}
	if _, err := os.Stat(report.SessionFile); err != nil {
		t.Fatal(err)
	}
}
