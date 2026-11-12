// bridge.go — the OpenAI-compatible HTTP bridge in front of the local `claude`
// CLI: /v1/chat/completions, /v1/responses, /v1/models, /health. Translates
// OpenAI (incl. Codex's custom_tool_call / namespaced tools) to `claude -p`.
// Embedded in the claude-codex binary and run in-process on 127.0.0.1.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func regexFencedBlock() *regexp.Regexp {
	return regexp.MustCompile("(?s)```(?:tool_call|json)?\\s*(.*?)```")
}

// debugLogRequestBody dumps raw incoming JSON to /tmp when DEBUG_LOG_BODY is
// set, for inspecting request shapes (e.g. Codex's namespaced tool entries)
// this bridge doesn't fully understand yet. No-op otherwise.
func debugLogRequestBody(tag string, raw []byte) {
	if os.Getenv("DEBUG_LOG_BODY") == "" {
		return
	}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("claude-bridge-debug-%s-%d.json", tag, time.Now().UnixNano()))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		log.Printf("debugLogRequestBody: write failed: %v", err)
		return
	}
	log.Printf("debugLogRequestBody: wrote %s (%d bytes)", path, len(raw))
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var (
	claudeBin        = envOr("CLAUDE_BIN", "claude")
	defaultModel     = envOr("DEFAULT_MODEL", "sonnet")
	requestTimeout   = envDuration("REQUEST_TIMEOUT_MS", 600000)
	streamChunkDelay = envDuration("STREAM_CHUNK_DELAY_MS", 15)
	workdir          string
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallbackMs int) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return time.Duration(fallbackMs) * time.Millisecond
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return time.Duration(fallbackMs) * time.Millisecond
	}
	return time.Duration(n) * time.Millisecond
}

func mapModel(requested string) string {
	m := strings.ToLower(strings.TrimSpace(requested))
	// A full Claude model id (e.g. claude-opus-4-5) passes straight to
	// `claude --model` so an exact version is honoured, not collapsed to a tier.
	if strings.HasPrefix(m, "claude-") {
		return strings.TrimSpace(requested)
	}
	switch {
	case strings.Contains(m, "opus"):
		return "opus"
	case strings.Contains(m, "haiku"):
		return "haiku"
	case strings.Contains(m, "sonnet"):
		return "sonnet"
	default:
		return defaultModel
	}
}

// ---- shared chat-message model -------------------------------------------

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type toolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCall struct {
	ID       string     `json:"id,omitempty"`
	Type     string     `json:"type,omitempty"`
	Function toolCallFn `json:"function"`

	// Set when the invoked tool is a Codex "custom" tool (e.g. functions.exec),
	// whose call item is custom_tool_call with a raw-text body, not a
	// function_call with JSON arguments.
	IsCustom  bool   `json:"-"`
	Namespace string `json:"-"`
	BareName  string `json:"-"` // tool name without the namespace prefix
	RawInput  string `json:"-"` // raw body for a custom tool (JS source for exec)
}

type toolDef struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Format      json.RawMessage `json:"format,omitempty"` // Codex "custom" tools carry their schema here
	Function    *toolDefFn      `json:"function,omitempty"`

	isCustom  bool   // derived: sub-tool type was "custom"
	namespace string // derived: owning namespace (e.g. "functions")
	bareName  string // derived: tool name without namespace prefix
}

type toolDefFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func (t toolDef) fn() (name, desc string, params json.RawMessage) {
	if t.Function != nil {
		return t.Function.Name, t.Function.Description, t.Function.Parameters
	}
	p := t.Parameters
	if len(p) == 0 {
		p = t.Format
	}
	return t.Name, t.Description, p
}

// additionalToolsItem is the input-stream entry Codex uses to ship its tool
// catalogue (type:"additional_tools"), whose .tools holds namespaces
// (type:"namespace", each with its own nested .tools).
type additionalToolsItem struct {
	Type  string `json:"type"`
	Tools []struct {
		Type        string    `json:"type"`
		Name        string    `json:"name"`
		Description string    `json:"description"`
		Tools       []toolDef `json:"tools"`
	} `json:"tools"`
}

// extractAdditionalTools flattens Codex's namespaced tool catalogue from the
// input stream into flat toolDefs named "<namespace>.<tool>" — the exact name
// the model must emit and that Codex expects back in each function_call.
func extractAdditionalTools(input json.RawMessage) []toolDef {
	if len(input) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil
	}
	var out []toolDef
	for _, raw := range items {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &probe) != nil || probe.Type != "additional_tools" {
			continue
		}
		var at additionalToolsItem
		if json.Unmarshal(raw, &at) != nil {
			continue
		}
		for _, ns := range at.Tools {
			if len(ns.Tools) > 0 {
				for _, sub := range ns.Tools {
					name, desc, params := sub.fn()
					out = append(out, toolDef{
						Name: ns.Name + "." + name, Description: desc, Parameters: params,
						isCustom: sub.Type == "custom", namespace: ns.Name, bareName: name,
					})
				}
			} else {
				out = append(out, toolDef{Name: ns.Name, Description: ns.Description, namespace: ns.Name, bareName: ns.Name})
			}
		}
	}
	return out
}

func contentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err == nil {
		var out []string
		for _, p := range parts {
			if t, ok := p["text"].(string); ok {
				out = append(out, t)
			} else if typ, ok := p["type"].(string); ok {
				out = append(out, fmt.Sprintf("[%s]", typ))
			}
		}
		return strings.Join(out, "\n")
	}
	return string(raw)
}

func buildPrompt(messages []chatMessage) (systemPrompt, transcript string) {
	var sysParts, turns []string
	for _, m := range messages {
		switch m.Role {
		case "system", "developer":
			sysParts = append(sysParts, contentToText(m.Content))
		case "user":
			turns = append(turns, "Human: "+contentToText(m.Content))
		case "assistant":
			if len(m.ToolCalls) > 0 {
				var calls []string
				for _, tc := range m.ToolCalls {
					calls = append(calls, fmt.Sprintf("%s(%s)", tc.Function.Name, tc.Function.Arguments))
				}
				turns = append(turns, "Assistant: [called tool(s): "+strings.Join(calls, ", ")+"]")
			} else {
				turns = append(turns, "Assistant: "+contentToText(m.Content))
			}
		case "tool":
			id := m.ToolCallID
			if id == "" {
				id = "unknown"
			}
			turns = append(turns, fmt.Sprintf("Tool Result (id %s): %s", id, contentToText(m.Content)))
		default:
			turns = append(turns, m.Role+": "+contentToText(m.Content))
		}
	}
	return strings.Join(sysParts, "\n\n"), strings.Join(turns, "\n\n")
}

func toolInstructions(tools []toolDef) string {
	if len(tools) == 0 {
		return ""
	}
	var catalogue []string
	hasCustom := false
	for _, t := range tools {
		name, desc, params := t.fn()
		if t.isCustom {
			hasCustom = true
			catalogue = append(catalogue, fmt.Sprintf("- %s (freeform): %s", name, desc))
			continue
		}
		p := "{}"
		if len(params) > 0 {
			p = string(params)
		}
		catalogue = append(catalogue, fmt.Sprintf("- %s: %s\n  parameters schema: %s", name, desc, p))
	}

	instr := "\n" +
		"You may call at most one of the following tools per turn.\n" +
		"To call a tool, reply with ONLY a single fenced code block, nothing else:\n" +
		"```tool_call\n" +
		`{"name": "<tool name>", "arguments": { ... }}` + "\n" +
		"```\n" +
		"The \"arguments\" object must satisfy that tool's parameters schema.\n"
	if hasCustom {
		instr += "\n" +
			"A tool marked (freeform) takes a single raw-text body, NOT JSON arguments. Call it with:\n" +
			"```tool_call\n" +
			`{"name": "<tool name>", "input": "<the raw body as a JSON-encoded string>"}` + "\n" +
			"```\n" +
			"For the exec tool the body is raw JavaScript that calls nested tools, e.g. " +
			`input: "const r = await tools.exec_command({cmd: \"ls\"}); text(r.output);"` + "\n"
	}
	instr += "If no tool call is needed, reply normally in plain text with no code fence.\n\n" +
		"Available tools:\n" + strings.Join(catalogue, "\n")
	return instr
}

var fencedBlockRe = regexFencedBlock()

func extractToolCall(text string, tools []toolDef) []toolCall {
	if len(tools) == 0 {
		// Known tool catalogue is empty (e.g. a namespaced tool structure we
		// don't parse yet) — still try the native XML format, ungated.
		return extractInvokeXML(text)
	}
	reg := map[string]toolDef{}
	for _, t := range tools {
		n, _, _ := t.fn()
		reg[n] = t
	}

	candidates := []string{}
	if m := fencedBlockRe.FindStringSubmatch(text); m != nil {
		candidates = append(candidates, strings.TrimSpace(m[1]))
	}
	candidates = append(candidates, strings.TrimSpace(text))

	type singleCall struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
	}
	type multiCall struct {
		ToolCalls []singleCall `json:"tool_calls"`
	}

	build := func(sc singleCall) (toolCall, bool) {
		def, ok := reg[sc.Name]
		if !ok {
			return toolCall{}, false
		}
		if def.isCustom {
			return toolCall{
				ID: newCallID(), Type: "custom", IsCustom: true,
				Namespace: def.namespace, BareName: def.bareName,
				RawInput: rawInputString(sc.Input),
				Function: toolCallFn{Name: sc.Name, Arguments: rawInputString(sc.Input)},
			}, true
		}
		return toolCall{ID: newCallID(), Type: "function", Function: toolCallFn{Name: sc.Name, Arguments: argsOrEmpty(sc.Arguments)}}, true
	}

	for _, c := range candidates {
		var multi multiCall
		if err := json.Unmarshal([]byte(c), &multi); err == nil && len(multi.ToolCalls) > 0 {
			var valid []toolCall
			for _, sc := range multi.ToolCalls {
				if tc, ok := build(sc); ok {
					valid = append(valid, tc)
				}
			}
			if len(valid) > 0 {
				return valid
			}
		}
		var single singleCall
		if err := json.Unmarshal([]byte(c), &single); err == nil {
			if tc, ok := build(single); ok {
				return []toolCall{tc}
			}
		}
	}

	if calls := extractInvokeXML(text); calls != nil {
		return calls
	}
	return nil
}

// rawInputString returns the raw body of a custom tool call. The model is
// told to send "input" as a JSON-encoded string, so a JSON string unwraps to
// its text; anything else is passed through verbatim.
func rawInputString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

var invokeBlockRe = regexp.MustCompile(`(?s)<invoke\s+name="([^"]+)">(.*?)</invoke>`)
var invokeParamRe = regexp.MustCompile(`(?s)<parameter\s+name="([^"]+)">(.*?)</parameter>`)

// extractInvokeXML catches Claude's own native tool-use syntax
// (<invoke name="..."><parameter name="...">value</parameter></invoke>),
// which the model falls back to on its own once given enough tools that the
// injected "```tool_call``` JSON" instruction stops being followed. Unlike
// the JSON path, this isn't gated against the known tool-name set: the
// caller's namespaced tool catalogue (Codex groups tools under
// type:"namespace") isn't fully understood yet, so any tool name the model
// invokes is trusted and passed through as-is.
func extractInvokeXML(text string) []toolCall {
	matches := invokeBlockRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	var calls []toolCall
	for _, m := range matches {
		name := strings.TrimSpace(m[1])
		body := m[2]
		args := map[string]any{}
		for _, p := range invokeParamRe.FindAllStringSubmatch(body, -1) {
			key := strings.TrimSpace(p[1])
			val := strings.TrimSpace(p[2])
			var parsed any
			if err := json.Unmarshal([]byte(val), &parsed); err == nil {
				args[key] = parsed
			} else {
				args[key] = val
			}
		}
		argsJSON, _ := json.Marshal(args)
		calls = append(calls, toolCall{ID: newCallID(), Type: "function", Function: toolCallFn{Name: name, Arguments: string(argsJSON)}})
	}
	return calls
}

func argsOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

func newCallID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "call_" + hex.EncodeToString(b)
}

// ---- claude CLI invocation -------------------------------------------------

type claudeResult struct {
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	SessionID    string  `json:"session_id"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func runClaude(systemPrompt, transcript, model string) (*claudeResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	args := []string{"-p", "--output-format", "json", "--tools", "", "--model", model}
	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}

	cmd := exec.CommandContext(ctx, claudeBin, args...)
	cmd.Dir = workdir
	cmd.Env = os.Environ()
	cmd.Stdin = strings.NewReader(nonEmpty(transcript))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("claude CLI timed out after %s", requestTimeout)
	}
	if err != nil && stdout.Len() == 0 {
		return nil, fmt.Errorf("claude CLI exited: %w: %s", err, truncate(stderr.String(), 2000))
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	last := lines[len(lines)-1]
	var result claudeResult
	if err := json.Unmarshal([]byte(last), &result); err != nil {
		return nil, fmt.Errorf("failed to parse claude CLI output: %w: %s", err, truncate(stdout.String(), 2000))
	}
	return &result, nil
}

func nonEmpty(s string) string {
	if s == "" {
		return " "
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---- Chat Completions endpoint --------------------------------------------

type chatCompletionsRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []toolDef     `json:"tools"`
	Stream   bool          `json:"stream"`
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var body chatCompletionsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	model := mapModel(body.Model)
	log.Printf("chat/completions requested model=%s -> %s, messages=%d, tools=%d, stream=%v",
		body.Model, model, len(body.Messages), len(body.Tools), body.Stream)

	systemPrompt, transcript := buildPrompt(body.Messages)
	fullSystemPrompt := strings.TrimSpace(systemPrompt + toolInstructions(body.Tools))

	result, err := runClaude(fullSystemPrompt, transcript, model)
	if err != nil {
		log.Printf("claude CLI error: %v", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("claude CLI done: cost=$%v, tokens_in=%d, tokens_out=%d", result.TotalCostUSD, result.Usage.InputTokens, result.Usage.OutputTokens)

	if result.IsError {
		writeError(w, http.StatusBadGateway, orDefault(result.Result, "claude CLI returned an error"))
		return
	}

	toolCalls := extractToolCall(result.Result, body.Tools)
	usage := map[string]int{
		"prompt_tokens":     result.Usage.InputTokens,
		"completion_tokens": result.Usage.OutputTokens,
		"total_tokens":      result.Usage.InputTokens + result.Usage.OutputTokens,
	}
	id := "chatcmpl-" + orDefault(result.SessionID, newUUID())
	created := time.Now().Unix()

	if body.Stream {
		streamChatCompletions(w, id, created, body.Model, result.Result, toolCalls, usage)
		return
	}

	message := map[string]any{"role": "assistant", "content": result.Result}
	finishReason := "stop"
	if toolCalls != nil {
		message = map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls}
		finishReason = "tool_calls"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   body.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": usage,
	})
}

func streamChatCompletions(w http.ResponseWriter, id string, created int64, model, text string, toolCalls []toolCall, usage map[string]int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	base := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model}
	sseJSON(w, flusher, merge(base, map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}}))

	finishReason := "stop"
	if toolCalls != nil {
		finishReason = "tool_calls"
		chunks := make([]map[string]any, len(toolCalls))
		for i, tc := range toolCalls {
			chunks[i] = map[string]any{"index": i, "id": tc.ID, "type": tc.Type, "function": tc.Function}
		}
		sseJSON(w, flusher, merge(base, map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"tool_calls": chunks}, "finish_reason": nil}}}))
	} else {
		for _, word := range splitKeepDelim(text) {
			if word == "" {
				continue
			}
			sseJSON(w, flusher, merge(base, map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": word}, "finish_reason": nil}}}))
			if streamChunkDelay > 0 {
				time.Sleep(streamChunkDelay)
			}
		}
	}

	sseJSON(w, flusher, merge(base, map[string]any{"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}}, "usage": usage}))
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ---- Responses API endpoint -------------------------------------------------

type responsesRequest struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions"`
	Input        json.RawMessage `json:"input"`
	Tools        []toolDef       `json:"tools"`
	Stream       bool            `json:"stream"`
}

type responsesInputItem struct {
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	Type    string          `json:"type,omitempty"`
	Name    string          `json:"name,omitempty"`
	Args    string          `json:"arguments,omitempty"`
	Input   string          `json:"input,omitempty"`  // custom_tool_call raw body
	CallID  string          `json:"call_id,omitempty"`
	Output  json.RawMessage `json:"output,omitempty"` // string, or a list of input_text parts
}

// outputToText flattens a tool-output field that may be a plain string or a
// list of content parts ({type:"input_text"|"output_text", text:...}), the
// latter being how Codex returns custom_tool_call_output.
func outputToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err == nil {
		var out []string
		for _, p := range parts {
			if t, ok := p["text"].(string); ok {
				out = append(out, t)
			}
		}
		return strings.Join(out, "\n")
	}
	return string(raw)
}

func responsesInputToMessages(body responsesRequest) []chatMessage {
	var messages []chatMessage
	if body.Instructions != "" {
		b, _ := json.Marshal(body.Instructions)
		messages = append(messages, chatMessage{Role: "system", Content: b})
	}

	if len(body.Input) == 0 {
		return messages
	}
	var asString string
	if err := json.Unmarshal(body.Input, &asString); err == nil {
		b, _ := json.Marshal(asString)
		return append(messages, chatMessage{Role: "user", Content: b})
	}

	var items []responsesInputItem
	if err := json.Unmarshal(body.Input, &items); err != nil {
		return messages
	}
	for _, it := range items {
		switch {
		case it.Role != "":
			messages = append(messages, chatMessage{Role: it.Role, Content: it.Content})
		case it.Type == "function_call":
			messages = append(messages, chatMessage{
				Role:      "assistant",
				ToolCalls: []toolCall{{Function: toolCallFn{Name: it.Name, Arguments: it.Args}}},
			})
		case it.Type == "custom_tool_call":
			messages = append(messages, chatMessage{
				Role:      "assistant",
				ToolCalls: []toolCall{{Function: toolCallFn{Name: it.Name, Arguments: it.Input}}},
			})
		case it.Type == "function_call_output" || it.Type == "custom_tool_call_output":
			b, _ := json.Marshal(outputToText(it.Output))
			messages = append(messages, chatMessage{Role: "tool", ToolCallID: it.CallID, Content: b})
		}
	}
	return messages
}

func handleResponses(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	debugLogRequestBody("responses", raw)

	var body responsesRequest
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	model := mapModel(body.Model)

	// Codex ships its tool catalogue inside the input stream (additional_tools),
	// not as top-level body.Tools. Merge both so the model is told about them.
	tools := append([]toolDef{}, body.Tools...)
	tools = append(tools, extractAdditionalTools(body.Input)...)
	log.Printf("v1/responses requested model=%s -> %s, tools=%d (top=%d), stream=%v", body.Model, model, len(tools), len(body.Tools), body.Stream)

	messages := responsesInputToMessages(body)
	systemPrompt, transcript := buildPrompt(messages)
	fullSystemPrompt := strings.TrimSpace(systemPrompt + toolInstructions(tools))

	result, err := runClaude(fullSystemPrompt, transcript, model)
	if err != nil {
		log.Printf("claude CLI error: %v", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("claude CLI done: cost=$%v, tokens_in=%d, tokens_out=%d", result.TotalCostUSD, result.Usage.InputTokens, result.Usage.OutputTokens)

	if result.IsError {
		writeError(w, http.StatusBadGateway, orDefault(result.Result, "claude CLI returned an error"))
		return
	}

	toolCalls := extractToolCall(result.Result, tools)
	usage := map[string]int{
		"input_tokens":  result.Usage.InputTokens,
		"output_tokens": result.Usage.OutputTokens,
		"total_tokens":  result.Usage.InputTokens + result.Usage.OutputTokens,
	}
	id := "resp_" + orDefault(result.SessionID, newUUID())
	created := time.Now().Unix()

	var output []map[string]any
	if toolCalls != nil {
		for _, tc := range toolCalls {
			output = append(output, toolCallOutputItem(tc))
		}
	} else {
		output = []map[string]any{{
			"type": "message", "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": result.Result, "annotations": []any{}}},
		}}
	}

	payload := map[string]any{
		"id": id, "object": "response", "created_at": created, "status": "completed",
		"model": body.Model, "output": output, "usage": usage,
	}
	if toolCalls == nil {
		payload["output_text"] = result.Result
	}

	if body.Stream {
		streamResponses(w, payload, result.Result, toolCalls)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// toolCallOutputItem renders a tool call as its non-streaming output item:
// custom_tool_call (raw-text input) for Codex custom tools, function_call
// (JSON arguments) otherwise.
func toolCallOutputItem(tc toolCall) map[string]any {
	if tc.IsCustom {
		item := map[string]any{
			"type": "custom_tool_call", "id": "ctc_" + newUUID(), "call_id": tc.ID,
			"name": tc.BareName, "input": tc.RawInput, "status": "completed",
		}
		if tc.Namespace != "" {
			item["namespace"] = tc.Namespace
		}
		return item
	}
	return map[string]any{
		"type": "function_call", "call_id": tc.ID, "name": tc.Function.Name,
		"arguments": tc.Function.Arguments, "status": "completed",
	}
}

func streamFunctionCall(w http.ResponseWriter, flusher http.Flusher, i int, tc toolCall) {
	itemID := "fc_" + newUUID()
	added := map[string]any{
		"type": "function_call", "id": itemID, "call_id": tc.ID,
		"name": tc.Function.Name, "arguments": "", "status": "in_progress",
	}
	sseNamedEvent(w, flusher, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": i, "item": added,
	})
	sseNamedEvent(w, flusher, "response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": itemID,
		"output_index": i, "delta": tc.Function.Arguments,
	})
	sseNamedEvent(w, flusher, "response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": itemID,
		"output_index": i, "arguments": tc.Function.Arguments,
	})
	done := merge(added, map[string]any{"status": "completed", "arguments": tc.Function.Arguments})
	sseNamedEvent(w, flusher, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": i, "item": done,
	})
}

// streamCustomToolCall emits the custom_tool_call lifecycle Codex requires for
// its freeform tools (e.g. functions.exec): item added, raw input streamed via
// custom_tool_call_input.delta/done, item done.
func streamCustomToolCall(w http.ResponseWriter, flusher http.Flusher, i int, tc toolCall) {
	itemID := "ctc_" + newUUID()
	added := map[string]any{
		"type": "custom_tool_call", "id": itemID, "call_id": tc.ID,
		"name": tc.BareName, "input": "", "status": "in_progress",
	}
	if tc.Namespace != "" {
		added["namespace"] = tc.Namespace
	}
	sseNamedEvent(w, flusher, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": i, "item": added,
	})
	sseNamedEvent(w, flusher, "response.custom_tool_call_input.delta", map[string]any{
		"type": "response.custom_tool_call_input.delta", "item_id": itemID,
		"output_index": i, "delta": tc.RawInput,
	})
	sseNamedEvent(w, flusher, "response.custom_tool_call_input.done", map[string]any{
		"type": "response.custom_tool_call_input.done", "item_id": itemID,
		"output_index": i, "input": tc.RawInput,
	})
	done := merge(added, map[string]any{"status": "completed", "input": tc.RawInput})
	sseNamedEvent(w, flusher, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": i, "item": done,
	})
}

func streamResponses(w http.ResponseWriter, payload map[string]any, text string, toolCalls []toolCall) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	inProgress := merge(payload, map[string]any{"status": "in_progress", "output": []any{}})
	sseNamedEvent(w, flusher, "response.created", map[string]any{"type": "response.created", "response": inProgress})

	hasToolCalls := len(toolCalls) > 0
	if hasToolCalls {
		for i, tc := range toolCalls {
			if tc.IsCustom {
				streamCustomToolCall(w, flusher, i, tc)
			} else {
				streamFunctionCall(w, flusher, i, tc)
			}
		}
	}

	itemID := "msg_" + newUUID()
	if !hasToolCalls {
		addedItem := map[string]any{
			"type": "message", "id": itemID, "status": "in_progress", "role": "assistant",
			"content": []any{},
		}
		sseNamedEvent(w, flusher, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0, "item": addedItem,
		})
		sseNamedEvent(w, flusher, "response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": itemID, "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})

		for _, word := range splitKeepDelim(text) {
			if word == "" {
				continue
			}
			sseNamedEvent(w, flusher, "response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": itemID, "output_index": 0, "content_index": 0, "delta": word,
			})
			if streamChunkDelay > 0 {
				time.Sleep(streamChunkDelay)
			}
		}

		sseNamedEvent(w, flusher, "response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": itemID, "output_index": 0, "content_index": 0, "text": text,
		})
		sseNamedEvent(w, flusher, "response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": itemID, "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		})
		doneItem := map[string]any{
			"type": "message", "id": itemID, "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		}
		sseNamedEvent(w, flusher, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0, "item": doneItem,
		})
	}

	sseNamedEvent(w, flusher, "response.completed", map[string]any{"type": "response.completed", "response": payload})
}

// ---- small helpers ----------------------------------------------------------

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func merge(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func splitKeepDelim(s string) []string {
	// Mirrors JS `text.split(/(\s+)/)`: keeps whitespace runs as their own tokens.
	var out []string
	var cur strings.Builder
	var curIsSpace bool
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		isSpace := r == ' ' || r == '\t' || r == '\n' || r == '\r'
		if cur.Len() > 0 && isSpace != curIsSpace {
			flush()
		}
		curIsSpace = isSpace
		cur.WriteRune(r)
	}
	flush()
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": "bridge_error"}})
}

func sseJSON(w http.ResponseWriter, f http.Flusher, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", b)
	f.Flush()
}

func sseNamedEvent(w http.ResponseWriter, f http.Flusher, event string, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	f.Flush()
}

// requireAuth rejects requests missing a matching "Authorization: Bearer
// <token>" header. A no-op when BRIDGE_AUTH_TOKEN is unset, so purely local
// use (PentAGI, NL2SQL over 127.0.0.1) keeps working unauthenticated.
func requireAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	if token == "" {
		return next
	}
	expected := "Bearer " + token
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != expected {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next(w, r)
	}
}

// bridgeHandler builds the HTTP mux. token is "" for local-only (no auth),
// which is the claude-codex default since the bridge binds 127.0.0.1 and lives
// only for the duration of one command.
func bridgeHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", requireAuth(token, handleChatCompletions))
	mux.HandleFunc("/chat/completions", requireAuth(token, handleChatCompletions))
	mux.HandleFunc("/v1/responses", requireAuth(token, handleResponses))
	mux.HandleFunc("/responses", requireAuth(token, handleResponses))
	mux.HandleFunc("/v1/models", requireAuth(token, handleModels))
	mux.HandleFunc("/models", requireAuth(token, handleModels))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "claude-codex"})
	})
	return mux
}

// initWorkdir creates the neutral cwd the spawned `claude` CLI runs in, so no
// project CLAUDE.md leaks into agent output.
func initWorkdir() error {
	workdir = filepath.Join(os.TempDir(), "claude-codex-workdir")
	return os.MkdirAll(workdir, 0o755)
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	var data []map[string]any
	for _, id := range []string{"sonnet", "opus", "haiku"} {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "claude-code-bridge-go"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

