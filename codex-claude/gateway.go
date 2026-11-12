// gateway.go — a minimal Anthropic Messages API (/v1/messages) server. Claude
// Code is pointed at it with ANTHROPIC_BASE_URL; it translates each request to
// the canonical model, asks the configured backend, and answers in Anthropic
// format (JSON or SSE). It binds 127.0.0.1 only and requires a per-session
// bearer token so other local software or web pages cannot spend the key.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type gateway struct {
	be    backend
	token string // required on /v1/* (Authorization: Bearer or x-api-key)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newToken() string { return randHex(24) }

func (g *gateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "codex-claude", "backend": g.be.Name()})
	})
	mux.HandleFunc("/v1/messages/count_tokens", g.auth(g.handleCountTokens))
	mux.HandleFunc("/v1/messages", g.auth(g.handleMessages))
	mux.HandleFunc("/v1/models", g.auth(g.handleModels))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("unhandled %s %s", r.Method, r.URL.Path)
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "unsupported endpoint: "+r.URL.Path)
	})
	return logRequests(mux)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func (g *gateway) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.Header.Get("x-api-key")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(g.token)) != 1 {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid gateway token")
			return
		}
		next(w, r)
	}
}

// ---- request parsing -------------------------------------------------------

type wireRequest struct {
	Model      string          `json:"model"`
	System     json.RawMessage `json:"system"`
	Messages   []wireMessage   `json:"messages"`
	Tools      []wireTool      `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	MaxTokens  int             `json:"max_tokens"`
	Stream     bool            `json:"stream"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// rawText flattens a string-or-blocks field to plain text.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var parts []string
	for _, it := range items {
		if it.Type == "text" && it.Text != "" {
			parts = append(parts, it.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func parseBlocks(raw json.RawMessage) []block {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []block{{Type: "text", Text: s}}
	}
	var items []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var out []block
	for _, it := range items {
		switch it.Type {
		case "text":
			out = append(out, block{Type: "text", Text: it.Text})
		case "tool_use":
			out = append(out, block{Type: "tool_use", ID: it.ID, Name: it.Name, Input: it.Input})
		case "tool_result":
			out = append(out, block{Type: "tool_result", ToolUseID: it.ToolUseID, Content: rawText(it.Content), IsError: it.IsError})
		case "image":
			out = append(out, block{Type: "text", Text: "[image omitted: this gateway does not forward images]"})
		}
		// thinking / redacted_thinking and unknown block types are dropped.
	}
	return out
}

func parseToolChoice(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return ""
	}
	switch tc.Type {
	case "any":
		return "any"
	case "none":
		return "none"
	case "tool":
		return "tool:" + tc.Name
	default:
		return "auto"
	}
}

func parseRequest(body []byte) (chatRequest, error) {
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return chatRequest{}, fmt.Errorf("invalid JSON: %w", err)
	}
	req := chatRequest{
		Model:     w.Model,
		System:    rawText(w.System),
		MaxTokens: w.MaxTokens,
		Stream:    w.Stream,
	}
	for _, m := range w.Messages {
		req.Messages = append(req.Messages, message{Role: m.Role, Blocks: parseBlocks(m.Content)})
	}
	for _, t := range w.Tools {
		if t.Name == "" || len(t.InputSchema) == 0 {
			continue // server-side tools carry no schema and are not forwarded
		}
		req.Tools = append(req.Tools, toolDef{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
	}
	req.ToolChoice = parseToolChoice(w.ToolChoice)
	return req, nil
}

// ---- handlers --------------------------------------------------------------

func (g *gateway) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "use POST")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
		return
	}
	req, err := parseRequest(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	log.Printf("messages: model=%s stream=%v tools=%d messages=%d", req.Model, req.Stream, len(req.Tools), len(req.Messages))
	for i, m := range req.Messages {
		for j, b := range m.Blocks {
			log.Printf("  msg[%d] %s block[%d] %s: %q", i, m.Role, j, b.Type, truncate(b.Text+b.Name+b.Content, 70))
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	res, err := g.be.Complete(ctx, req)
	if err != nil {
		var berr *backendError
		if errors.As(err, &berr) {
			status := berr.Status
			if status == 0 {
				status = http.StatusBadGateway
			}
			writeAnthropicError(w, status, berr.Type, berr.Message)
			return
		}
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	id := "msg_" + randHex(12)
	if req.Stream {
		streamMessage(w, id, req.Model, res)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": req.Model,
		"content":     contentJSON(res.Blocks),
		"stop_reason": res.StopReason, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": res.InputTokens, "output_tokens": res.OutputTokens},
	})
}

// handleCountTokens returns a rough estimate (bytes/4); Claude Code only uses
// it for budgeting, so exactness is not needed.
func (g *gateway) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	n := len(body) / 4
	if n < 1 {
		n = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": n})
}

func (g *gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"data":     []map[string]any{{"type": "model", "id": "codex-claude", "display_name": "codex-claude (" + g.be.Name() + ")"}},
		"has_more": false,
	})
}

// ---- responses -------------------------------------------------------------

func nonEmptyJSON(raw json.RawMessage) json.RawMessage {
	if strings.TrimSpace(string(raw)) == "" {
		return json.RawMessage("{}")
	}
	return raw
}

func contentJSON(blocks []block) []map[string]any {
	out := []map[string]any{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": b.Text})
		case "tool_use":
			out = append(out, map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": nonEmptyJSON(b.Input)})
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"type": "text", "text": ""})
	}
	return out
}

// streamMessage replays a finished result as an Anthropic SSE stream.
func streamMessage(w http.ResponseWriter, id, model string, res chatResult) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": res.InputTokens, "output_tokens": 0},
	}})

	blocks := res.Blocks
	if len(blocks) == 0 {
		blocks = []block{{Type: "text"}}
	}
	idx := 0
	for _, b := range blocks {
		switch b.Type {
		case "text":
			send("content_block_start", map[string]any{"type": "content_block_start", "index": idx,
				"content_block": map[string]any{"type": "text", "text": ""}})
			for _, chunk := range chunkText(b.Text, 48) {
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx,
					"delta": map[string]any{"type": "text_delta", "text": chunk}})
			}
			send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
			idx++
		case "tool_use":
			send("content_block_start", map[string]any{"type": "content_block_start", "index": idx,
				"content_block": map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": map[string]any{}}})
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": string(nonEmptyJSON(b.Input))}})
			send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
			idx++
		}
	}

	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": res.StopReason, "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": res.OutputTokens}})
	send("message_stop", map[string]any{"type": "message_stop"})
}

func chunkText(s string, size int) []string {
	r := []rune(s)
	if len(r) == 0 {
		return []string{""}
	}
	var out []string
	for i := 0; i < len(r); i += size {
		end := i + size
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error", "error": map[string]any{"type": typ, "message": msg},
	})
}
