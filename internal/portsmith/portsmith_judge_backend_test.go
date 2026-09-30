package portsmith_test

import (
	"context"
	"encoding/json"
	"fmt"
	aitypes "github.com/minifish-org/pith/packages/ai/types"
	ps "github.com/minifish-org/portsmith-go/internal/portsmith"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func response(reason aitypes.StopReason, blocks ...aitypes.ContentBlock) *aitypes.AssistantMessageEventStream {
	s := aitypes.NewAssistantMessageEventStream()
	m := aitypes.NewAssistantMessage(aitypes.ApiOpenAICompletions, aitypes.ProviderOpenAI, "fixture", 1)
	m.Content = blocks
	m.StopReason = reason
	m.Usage = aitypes.Usage{Input: 10, Output: 5, TotalTokens: 15}
	s.Push(aitypes.NewDoneEvent(reason, m))
	return s
}
func call(id, name string, args any) aitypes.ContentBlock {
	b, _ := json.Marshal(args)
	c := aitypes.NewToolCall(id, name, b)
	return aitypes.ContentBlock{Type: aitypes.ContentTypeToolCall, ToolCall: &c}
}
func testModel() ps.ModelConfig {
	return ps.ModelConfig{ID: "deepseek-flash", BaseURL: "http://127.0.0.1:1/v1", APIKey: "fixture-secret-do-not-log", ContextWindow: 1000000, MaxTokens: 384000, Thinking: "high"}
}

func TestPortsmithJudgePithRepairAndResume(t *testing.T) {
	p := prepared(t)
	candidate(t, p, badCode)
	var n atomic.Int32
	var sawFailure, sawSuccess atomic.Bool
	opts := ps.RunOptions{Root: p, Model: testModel(), AgentDir: t.TempDir(), Feedback: "unique-resume-marker", MaxTurns: 12}
	opts.StreamFn = func(m *aitypes.Model, c *aitypes.TranscriptContext, o *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		if m.ContextWindow != 1000000 || m.MaxTokens != 384000 {
			t.Error("backend silently reduced capacity")
		}
		b, _ := json.Marshal(c)
		s := string(b)
		switch n.Add(1) {
		case 1:
			for _, tool := range []string{"read", "write", "edit", "bash", "grep", "find", "ls", "verify_candidate"} {
				if !strings.Contains(s, `"name":"`+tool+`"`) {
					t.Errorf("missing native tool %s", tool)
				}
			}
			return response(aitypes.StopReasonToolUse, call("verify-bad", "verify_candidate", map[string]any{}))
		case 2:
			sawFailure.Store(strings.Contains(s, "behavior_failed"))
			return response(aitypes.StopReasonToolUse, call("repair", "write", map[string]any{"path": "value.go", "content": goodCode}))
		case 3:
			return response(aitypes.StopReasonToolUse, call("verify-good", "verify_candidate", map[string]any{}))
		default:
			sawSuccess.Store(strings.Contains(s, "behavior_verified"))
			return response(aitypes.StopReasonStop, aitypes.TextBlock("migration candidate ready"))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	r, e := ps.RunPort(ctx, opts)
	must(t, e)
	if r.Status != "candidate_ready" || n.Load() != 4 || !sawFailure.Load() || !sawSuccess.Load() {
		t.Fatalf("did not repair in one Pith session: %+v calls=%d failure=%v success=%v", r, n.Load(), sawFailure.Load(), sawSuccess.Load())
	}
	if r.SessionFile == "" {
		t.Fatal("missing durable session path")
	}
	if _, e = os.Stat(r.SessionFile); e != nil {
		t.Fatal(e)
	}
	var restored atomic.Bool
	opts.StreamFn = func(_ *aitypes.Model, c *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		b, _ := json.Marshal(c)
		restored.Store(strings.Contains(string(b), "unique-resume-marker") && strings.Contains(string(b), "verify-good"))
		return response(aitypes.StopReasonStop, aitypes.TextBlock("resumed"))
	}
	opts.Feedback = "continue"
	r, e = ps.RunPort(ctx, opts)
	must(t, e)
	if !r.Resumed || !restored.Load() {
		t.Fatal("Pith conversation not restored", r)
	}
	must(t, filepath.WalkDir(p, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".jsonl") {
			if strings.Contains(string(raw(t, path)), opts.Model.APIKey) {
				t.Errorf("credential persisted in %s", path)
			}
		}
		return nil
	}))
}

func TestPortsmithJudgePithContinuationAndCancellation(t *testing.T) {
	p := prepared(t)
	candidate(t, p, goodCode)
	var calls atomic.Int32
	opts := ps.RunOptions{Root: p, Model: testModel(), AgentDir: t.TempDir(), MaxTurns: 5, StreamFn: func(_ *aitypes.Model, _ *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		if calls.Add(1) == 1 {
			return response(aitypes.StopReasonLength, call("incomplete", "write", map[string]any{"path": "must-not-exist.go", "content": "package port"}))
		}
		return response(aitypes.StopReasonStop, aitypes.TextBlock("done"))
	}}
	r, e := ps.RunPort(context.Background(), opts)
	must(t, e)
	if calls.Load() != 2 || r.Status != "candidate_ready" {
		t.Fatal("length response not continued", r, calls.Load())
	}
	if _, e = os.Stat(filepath.Join(p, "candidate/must-not-exist.go")); !os.IsNotExist(e) {
		t.Fatal("executed length-truncated tool call")
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	before := calls.Load()
	r, e = ps.RunPort(cancelled, opts)
	if e == nil && r.Status == "candidate_ready" {
		t.Fatal("cancelled run reported success")
	}
	if calls.Load() != before {
		t.Fatal("called provider after cancellation")
	}
	// Positive max-turn budget must preserve the session and stop a never-ending tool loop.
	opts.MaxTurns = 2
	opts.StreamFn = func(_ *aitypes.Model, _ *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		return response(aitypes.StopReasonToolUse, call("read-loop", "read", map[string]any{"path": "value.go"}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, e = ps.RunPort(ctx, opts)
	if r.Status == "candidate_ready" || ctx.Err() != nil {
		t.Fatal("max-turn budget not applied", r, e)
	}
}

func TestPortsmithJudgeNativeProviderHTTP(t *testing.T) {
	p := prepared(t)
	candidate(t, p, goodCode)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-secret-do-not-log" {
			t.Error("incorrect compatible provider request", r.URL.Path)
		}
		var body map[string]any
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		if body["model"] != "deepseek-flash" || body["max_tokens"] != float64(384000) {
			t.Error("lost model request settings", body["model"], body["max_tokens"])
		}
		offered, _ := body["tools"].([]any)
		if len(offered) < 8 {
			t.Error("provider request missing native tools")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model := testModel()
	model.BaseURL = server.URL + "/v1"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, e := ps.RunPort(ctx, ps.RunOptions{Root: p, Model: model, AgentDir: t.TempDir(), MaxTurns: 3})
	must(t, e)
	if requests.Load() != 1 || report.Status != "candidate_ready" {
		t.Fatal("native HTTP provider failed", requests.Load(), report)
	}
}
