// Command events subscribes to the public session event stream and prints each
// event in order. It runs offline with a scripted stream.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	agenttypes "github.com/minifish-org/pith/packages/agent/types"
	aitypes "github.com/minifish-org/pith/packages/ai/types"
	codingagent "github.com/minifish-org/pith/packages/coding-agent"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := codingagent.CreateAgentSession(codingagent.SessionOptions{
		Cwd:   mustGetwd(),
		Model: codingagent.ModelOptions{Model: fakeModel(), StreamFn: fakeStream("event example")},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "create session:", err)
		os.Exit(1)
	}
	defer session.Close()

	// Subscribe returns an idempotent unsubscribe function. Callbacks run
	// outside the session lock, so they may inspect snapshots or unsubscribe.
	unsubscribe := session.Subscribe(func(event codingagent.SessionEvent) {
		fmt.Printf("event type=%-24s tool=%-8s error=%v\n", event.Type, event.ToolName, event.IsError)
	})
	defer unsubscribe()

	if _, err := session.Prompt(ctx, "emit events"); err != nil {
		fmt.Fprintln(os.Stderr, "prompt:", err)
		os.Exit(1)
	}
	fmt.Println("subscribers after run:", session.SubscriberCount())
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

func fakeStream(text string) agenttypes.StreamFn {
	return func(model *aitypes.Model, _ *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		stream := aitypes.NewAssistantMessageEventStream()
		message := aitypes.NewAssistantMessage(model.Api, model.Provider, model.Id, 1)
		message.Content = []aitypes.ContentBlock{aitypes.TextBlock(text)}
		message.StopReason = aitypes.StopReasonStop
		message.Usage = aitypes.Usage{Input: 8, Output: 4, TotalTokens: 12}
		stream.Push(aitypes.NewDoneEvent(aitypes.StopReasonStop, message))
		return stream
	}
}
