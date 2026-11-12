// types.go — the canonical, Anthropic-shaped chat model shared by the gateway
// (which speaks the Anthropic Messages API to Claude Code) and the backends
// (which speak to a real or fake model).
package main

import (
	"context"
	"encoding/json"
)

// block is one content block of a message.
type block struct {
	Type      string          // "text" | "tool_use" | "tool_result"
	Text      string          // text
	ID        string          // tool_use: call id
	Name      string          // tool_use: tool name
	Input     json.RawMessage // tool_use: JSON object of arguments
	ToolUseID string          // tool_result: the tool_use id it answers
	Content   string          // tool_result: flattened text output
	IsError   bool            // tool_result
}

type message struct {
	Role   string // "user" | "assistant"
	Blocks []block
}

type toolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema of the tool input
}

type chatRequest struct {
	Model      string
	System     string
	Messages   []message
	Tools      []toolDef
	ToolChoice string // "" | "auto" | "any" | "none" | "tool:<name>"
	MaxTokens  int
	Stream     bool
}

type chatResult struct {
	Blocks       []block
	StopReason   string // end_turn | tool_use | max_tokens
	InputTokens  int
	OutputTokens int
}

// backend produces one assistant turn for a request.
type backend interface {
	Name() string
	Complete(ctx context.Context, req chatRequest) (chatResult, error)
}

// preflighter is optionally implemented by a backend to verify it is usable
// (reachable, credentials accepted) before a session starts.
type preflighter interface {
	Preflight(ctx context.Context) (string, error)
}

// backendError carries an upstream failure in a shape the gateway can hand
// back to Claude Code as an Anthropic-style error.
type backendError struct {
	Status  int    // HTTP status to return (0 = 502)
	Type    string // Anthropic error type, e.g. "rate_limit_error"
	Message string
}

func (e *backendError) Error() string { return e.Message }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// roughTokens is a bytes/4 estimate; Claude Code only uses token counts for
// budgeting, so exactness is not needed.
func roughTokens(s string) int {
	n := len(s) / 4
	if n < 1 {
		return 1
	}
	return n
}
