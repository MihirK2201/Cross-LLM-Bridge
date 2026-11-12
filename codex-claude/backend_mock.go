// backend_mock.go — a scripted fake model for harness testing. It needs no
// network and no credentials, and answers deterministically so the Claude Code
// loop (streaming, tool calls, tool results, errors, slowness) can be exercised
// and asserted on.
//
// Triggers (put them anywhere in the user's prompt):
//
//	[mock:tool]   emit a tool call (Bash `echo mock-tool-ran` if Bash is offered)
//	[mock:error]  fail the request with an invalid_request_error
//	[mock:slow]   wait ~8s before answering
//
// After a tool result it answers "MOCK: tool result received: <result>".
// Anything else is echoed back as "MOCK: <your prompt>".
package main

import (
	"context"
	"strings"
	"time"
)

type mockBackend struct{}

func (mockBackend) Name() string { return "mock" }

func (mockBackend) Preflight(ctx context.Context) (string, error) {
	return "Mock model ready (no network needed)", nil
}

func lastTextBlock(m message) string {
	for i := len(m.Blocks) - 1; i >= 0; i-- {
		if m.Blocks[i].Type == "text" && strings.TrimSpace(m.Blocks[i].Text) != "" {
			return m.Blocks[i].Text
		}
	}
	return ""
}

func allText(m message) string {
	var parts []string
	for _, b := range m.Blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (mockBackend) Complete(ctx context.Context, req chatRequest) (chatResult, error) {
	// Claude Code may append system-role messages (environment info) after the
	// user's turn; the turn to answer is the last non-system message.
	var last message
	found := false
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "system" {
			last, found = req.Messages[i], true
			break
		}
	}
	if !found {
		return chatResult{Blocks: []block{{Type: "text", Text: "MOCK: (no messages)"}}, StopReason: "end_turn"}, nil
	}
	in := roughTokens(allText(last))

	// A tool result is coming back: answer it.
	var results []string
	for _, b := range last.Blocks {
		if b.Type == "tool_result" {
			results = append(results, strings.TrimSpace(b.Content))
		}
	}
	if len(results) > 0 {
		text := "MOCK: tool result received: " + truncate(strings.Join(results, " | "), 300)
		return chatResult{Blocks: []block{{Type: "text", Text: text}}, StopReason: "end_turn", InputTokens: in, OutputTokens: roughTokens(text)}, nil
	}

	text := allText(last)
	if strings.Contains(text, "[mock:error]") {
		return chatResult{}, &backendError{Status: 400, Type: "invalid_request_error", Message: "mock error requested by [mock:error]"}
	}
	if strings.Contains(text, "[mock:slow]") {
		select {
		case <-time.After(8 * time.Second):
		case <-ctx.Done():
			return chatResult{}, ctx.Err()
		}
	}
	if strings.Contains(text, "[mock:tool]") && len(req.Tools) > 0 {
		tool := req.Tools[0]
		input := `{}`
		for _, t := range req.Tools {
			if t.Name == "Bash" {
				tool = t
				input = `{"command":"echo mock-tool-ran","description":"mock tool call"}`
				break
			}
		}
		return chatResult{
			Blocks:      []block{{Type: "tool_use", ID: "toolu_mock_" + randHex(4), Name: tool.Name, Input: []byte(input)}},
			StopReason:  "tool_use",
			InputTokens: in, OutputTokens: 20,
		}, nil
	}

	reply := "MOCK: " + truncate(strings.TrimSpace(lastTextBlock(last)), 300)
	return chatResult{Blocks: []block{{Type: "text", Text: reply}}, StopReason: "end_turn", InputTokens: in, OutputTokens: roughTokens(reply)}, nil
}
