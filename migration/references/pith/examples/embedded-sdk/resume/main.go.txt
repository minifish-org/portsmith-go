// Command resume demonstrates durable sessions: prompt, close, reopen the same
// session file and prompt again with the restored transcript.
//
// The caller owns the SessionManager when it is supplied, so the example closes
// it explicitly. The session file is a temp file unless PITH_SDK_SESSION_FILE
// points at a real path.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	aitypes "github.com/minifish-org/pith/packages/ai/types"
	codingagent "github.com/minifish-org/pith/packages/coding-agent"
)

// sessionFile is the durable file both runs open.
var sessionFile string

func main() {
	dir, err := os.MkdirTemp("", "pith-resume-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "temp dir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	sessionFile = os.Getenv("PITH_SDK_SESSION_FILE")
	if sessionFile == "" {
		sessionFile = filepath.Join(dir, "session.jsonl")
	}

	run("write the first note", func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		return fakeStream("recorded first note")
	})
	run("confirm the note is still there", func(_ *aitypes.Model, transcript *aitypes.TranscriptContext, _ *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream {
		if strings.Contains(marshal(transcript), "write the first note") {
			return fakeStream("the first note is still here")
		}
		return fakeStream("first note missing")
	})

	fmt.Println("session file:", sessionFile)
}

func run(prompt string, stream func(*aitypes.Model, *aitypes.TranscriptContext, *aitypes.SimpleStreamOptions) *aitypes.AssistantMessageEventStream) {
	cwd, _ := os.Getwd()
	manager, err := codingagent.OpenSession(sessionFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open session:", err)
		os.Exit(1)
	}
	defer manager.Close()

	session, err := codingagent.CreateAgentSession(codingagent.SessionOptions{
		Cwd:     cwd,
		Manager: manager,
		Model:   codingagent.ModelOptions{Model: fakeModel(), StreamFn: stream},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "create session:", err)
		os.Exit(1)
	}
	result, err := session.Prompt(context.Background(), prompt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "prompt:", err)
		os.Exit(1)
	}
	_ = session.Close()
	fmt.Printf("prompt=%q messages=%d leaf=%s\n", prompt, len(result.Messages), manager.LeafID())
}

func marshal(transcript *aitypes.TranscriptContext) string {
	data, err := json.Marshal(transcript)
	if err != nil {
		return ""
	}
	return string(data)
}

func fakeModel() *aitypes.Model {
	return &aitypes.Model{
		Id: "fake-model", Name: "fake-model",
		Api: aitypes.ApiOpenAICompletions, Provider: aitypes.ProviderOpenAI,
		BaseUrl: "http://localhost.invalid/v1", ContextWindow: 128000, MaxTokens: 4096,
	}
}

func fakeStream(text string) *aitypes.AssistantMessageEventStream {
	model := fakeModel()
	stream := aitypes.NewAssistantMessageEventStream()
	message := aitypes.NewAssistantMessage(model.Api, model.Provider, model.Id, 1)
	message.Content = []aitypes.ContentBlock{aitypes.TextBlock(text)}
	message.StopReason = aitypes.StopReasonStop
	message.Usage = aitypes.Usage{Input: 8, Output: 4, TotalTokens: 12}
	stream.Push(aitypes.NewDoneEvent(aitypes.StopReasonStop, message))
	return stream
}
