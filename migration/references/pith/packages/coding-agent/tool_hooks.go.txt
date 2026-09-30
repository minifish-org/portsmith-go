// Tool execution hooks for the embedded SDK.
//
// This file ports the hook contract that the source tool registry applied
// around every executable call. Hooks are application callbacks, not an OS
// sandbox: Before runs for every valid executable call and may deny it, After
// runs on executed results and may transform them. Hooks are always invoked
// outside any registry lock so a hook can safely call back into the registry.
//
// Upstream: Copyright (c) 2025 Mario Zechner, MIT License.
// See the repository LICENSE for the full text.
package codingagent

import "context"

// ToolHooks wraps tool execution. Both callbacks are optional.
type ToolHooks struct {
	// Before runs after schema validation and before execution. A non-nil
	// error denies the call and prevents the tool from running.
	Before func(ctx context.Context, call ToolCall) error
	// After runs on executed results and may transform them. A non-nil error
	// rejects the call.
	After func(ctx context.Context, call ToolCall, result ToolResult) (ToolResult, error)
}

// RunBefore invokes the Before hook when present.
func (h ToolHooks) RunBefore(ctx context.Context, call ToolCall) error {
	if h.Before == nil {
		return nil
	}
	return h.Before(ctx, call)
}

// RunAfter invokes the After hook when present and returns the transformed
// result.
func (h ToolHooks) RunAfter(ctx context.Context, call ToolCall, result ToolResult) (ToolResult, error) {
	if h.After == nil {
		return result, nil
	}
	return h.After(ctx, call, result)
}
