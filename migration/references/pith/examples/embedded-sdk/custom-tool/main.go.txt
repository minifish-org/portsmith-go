// Command custom-tool demonstrates a custom Go verification tool, execution
// hooks and a per-request credential callback.
//
// The example models a Portsmith-style verification: the tool reports a
// failure on the first call and the agent keeps going in the same conversation
// until verification passes. It runs entirely offline with a scripted stream.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	aitypes "github.com/minifish-org/pith/packages/ai/types"
	codingagent "github.com/minifish-org/pith/packages/coding-agent"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var verifications atomic.Int32
	tools, err := codingagent.NewToolRegistry("", []codingagent.ToolDefinition{{
		Name:        "verify_candidate",
		Description: "Verify the candidate and report the concrete result.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(context.Context, json.RawMessage) (codingagent.ToolResult, error) {
			if verifications.Add(1) == 1 {
				return codingagent.ToolResult{
					Content: []aitypes.ContentBlock{aitypes.TextBlock("verification failed: missing output")},
					IsError: true,
				}, nil
			}
			return codingagent.ToolResult{
				Content: []aitypes.ContentBlock{aitypes.TextBlock("offline-verification-passed")},
			}, nil
		},
	}}, []string{"verify_candidate"}, nil, codingagent.ToolHooks{
		Before: func(_ context.Context, call codingagent.ToolCall) error {
			fmt.Println("[hook] before", call.Name)
			return nil // return an error to deny the call
		},
		After: func(_ context.Context, call codingagent.ToolCall, result codingagent.ToolResult) (codingagent.ToolResult, error) {
			fmt.Printf("[hook] after %s isError=%v\n", call.Name, result.IsError)
			return result, nil
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "build tools:", err)
		os.Exit(1)
	}

	requests := 0
	stream := func(model *aitypes.Model, _ *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		requests++
		if requests <= 2 {
			return toolCallStream(model, fmt.Sprintf("verify-%d", requests), "verify_candidate", `{}`)
		}
		return textStream(model, "candidate repaired and verified")
	}

	session, err := codingagent.CreateAgentSession(codingagent.SessionOptions{
		Cwd:   mustGetwd(),
		Tools: tools,
		Model: codingagent.ModelOptions{
			Model:    fakeModel(),
			StreamFn: stream,
			APIKey: func(_ context.Context, provider string) (string, error) {
				fmt.Println("[credential] resolving key for", provider)
				return "offline-fixture-key", nil
			},
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "create session:", err)
		os.Exit(1)
	}
	defer session.Close()

	result, err := session.Prompt(ctx, "verify then repair the candidate")
	if err != nil {
		fmt.Fprintln(os.Stderr, "prompt:", err)
		os.Exit(1)
	}
	fmt.Printf("stop=%s verification_calls=%d\n", result.StopReason, verifications.Load())
	fmt.Println("final:", lastAssistantText(result))
}

func mustGetwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

func fakeModel() *aitypes.Model {
	return &aitypes.Model{
		Id: "fake-model", Name: "fake-model",
		Api: aitypes.ApiOpenAICompletions, Provider: aitypes.ProviderOpenAI,
		BaseUrl: "http://localhost.invalid/v1", ContextWindow: 128000, MaxTokens: 4096,
	}
}

func textStream(model *aitypes.Model, text string) *aitypes.AssistantMessageEventStream {
	stream := aitypes.NewAssistantMessageEventStream()
	message := aitypes.NewAssistantMessage(model.Api, model.Provider, model.Id, 1)
	message.Content = []aitypes.ContentBlock{aitypes.TextBlock(text)}
	message.StopReason = aitypes.StopReasonStop
	message.Usage = aitypes.Usage{Input: 8, Output: 4, TotalTokens: 12}
	stream.Push(aitypes.NewDoneEvent(aitypes.StopReasonStop, message))
	return stream
}

func toolCallStream(model *aitypes.Model, id, name, arguments string) *aitypes.AssistantMessageEventStream {
	stream := aitypes.NewAssistantMessageEventStream()
	message := aitypes.NewAssistantMessage(model.Api, model.Provider, model.Id, 1)
	call := aitypes.NewToolCall(id, name, json.RawMessage(arguments))
	message.Content = []aitypes.ContentBlock{{Type: aitypes.ContentTypeToolCall, ToolCall: &call}}
	message.StopReason = aitypes.StopReasonToolUse
	message.Usage = aitypes.Usage{Input: 8, Output: 4, TotalTokens: 12}
	stream.Push(aitypes.NewDoneEvent(aitypes.StopReasonToolUse, message))
	return stream
}

func lastAssistantText(result codingagent.RunResult) string {
	text := ""
	for _, message := range result.Messages {
		if message.Message == nil || message.Message.Assistant == nil {
			continue
		}
		for _, block := range message.Message.Assistant.Content {
			if block.IsText() && block.Text != nil {
				text = block.Text.Text
			}
		}
	}
	return text
}
