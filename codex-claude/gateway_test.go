package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- helpers ---------------------------------------------------------------

// stubBackend returns a canned result (or error) and records what it was asked.
type stubBackend struct {
	result chatResult
	err    error
	seen   []chatRequest
}

func (s *stubBackend) Name() string { return "stub" }
func (s *stubBackend) Complete(ctx context.Context, req chatRequest) (chatResult, error) {
	s.seen = append(s.seen, req)
	return s.result, s.err
}

func gatewayFor(t *testing.T, be backend) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer((&gateway{be: be, token: "tok"}).handler())
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, ts *httptest.Server, token, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var toolUseResult = chatResult{
	Blocks:       []block{{Type: "tool_use", ID: "call_9", Name: "Bash", Input: json.RawMessage(`{"command":"cat a.txt"}`)}},
	StopReason:   "tool_use",
	InputTokens:  11,
	OutputTokens: 7,
}

const toolLoopRequest = `{
  "model": "claude-sonnet-4-5", "max_tokens": 1000, "stream": %s,
  "system": [{"type": "text", "text": "be brief"}],
  "tools": [{"name": "Bash", "description": "run a command",
             "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}],
  "tool_choice": {"type": "auto"},
  "messages": [
    {"role": "user", "content": "list files"},
    {"role": "assistant", "content": [{"type": "text", "text": "ok"},
      {"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": {"command":"ls"}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": "a.txt"},
      {"type": "text", "text": "now summarize"}]}
  ]
}`

// ---- gateway ---------------------------------------------------------------

func TestGatewayParsesAnthropicRequestForTheBackend(t *testing.T) {
	be := &stubBackend{result: toolUseResult}
	ts := gatewayFor(t, be)

	resp, body := post(t, ts, "tok", strings.Replace(toolLoopRequest, "%s", "false", 1))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	got := be.seen[0]
	if got.System != "be brief" || got.MaxTokens != 1000 || got.ToolChoice != "auto" || got.Stream {
		t.Errorf("request fields wrong: %+v", got)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "Bash" || !strings.Contains(string(got.Tools[0].Schema), `"command"`) {
		t.Errorf("tools wrong: %+v", got.Tools)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d", len(got.Messages))
	}
	asst := got.Messages[1]
	if asst.Role != "assistant" || len(asst.Blocks) != 2 || asst.Blocks[0].Text != "ok" ||
		asst.Blocks[1].Type != "tool_use" || asst.Blocks[1].ID != "toolu_1" || string(asst.Blocks[1].Input) != `{"command":"ls"}` {
		t.Errorf("assistant turn wrong: %+v", asst)
	}
	user := got.Messages[2]
	if len(user.Blocks) != 2 || user.Blocks[0].Type != "tool_result" || user.Blocks[0].ToolUseID != "toolu_1" ||
		user.Blocks[0].Content != "a.txt" || user.Blocks[1].Text != "now summarize" {
		t.Errorf("user turn wrong: %+v", user)
	}

	var out struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("bad response JSON: %v\n%s", err, body)
	}
	if out.StopReason != "tool_use" || len(out.Content) != 1 {
		t.Fatalf("unexpected response: %s", body)
	}
	if c := out.Content[0]; c.Type != "tool_use" || c.ID != "call_9" || c.Name != "Bash" || c.Input["command"] != "cat a.txt" {
		t.Errorf("tool_use block wrong: %+v", c)
	}
	if out.Usage.In != 11 || out.Usage.Out != 7 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

func TestStreamingToolUseEvents(t *testing.T) {
	ts := gatewayFor(t, &stubBackend{result: toolUseResult})
	resp, body := post(t, ts, "tok", strings.Replace(toolLoopRequest, "%s", "true", 1))
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	order := []string{"event: message_start", "event: content_block_start", "event: content_block_delta",
		"event: content_block_stop", "event: message_delta", "event: message_stop"}
	pos := 0
	for _, want := range order {
		i := strings.Index(body[pos:], want)
		if i < 0 {
			t.Fatalf("missing or out-of-order %q in stream:\n%s", want, body)
		}
		pos += i + len(want)
	}
	for _, want := range []string{`"input_json_delta"`, `cat a.txt`, `"stop_reason":"tool_use"`, `"type":"tool_use"`} {
		if !strings.Contains(body, want) {
			t.Errorf("stream lacks %s:\n%s", want, body)
		}
	}
}

func TestStreamingTextEvents(t *testing.T) {
	be := &stubBackend{result: chatResult{Blocks: []block{{Type: "text", Text: "hello there"}}, StopReason: "end_turn"}}
	ts := gatewayFor(t, be)
	_, body := post(t, ts, "tok", `{"model":"m","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	for _, want := range []string{`"text_delta"`, `hello there`, `"stop_reason":"end_turn"`} {
		if !strings.Contains(body, want) {
			t.Errorf("stream lacks %s:\n%s", want, body)
		}
	}
}

func TestBackendErrorsBecomeAnthropicErrors(t *testing.T) {
	cases := []struct {
		err    error
		status int
		typ    string
	}{
		{&backendError{Status: 429, Type: "rate_limit_error", Message: "slow down"}, 429, "rate_limit_error"},
		{&backendError{Status: 401, Type: "authentication_error", Message: "log in"}, 401, "authentication_error"},
		{errors.New("plain failure"), 502, "api_error"},
	}
	for _, c := range cases {
		ts := gatewayFor(t, &stubBackend{err: c.err})
		resp, body := post(t, ts, "tok", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
		if resp.StatusCode != c.status || !strings.Contains(body, `"type":"error"`) ||
			!strings.Contains(body, c.typ) || !strings.Contains(body, c.err.Error()) {
			t.Errorf("%v: status %d body %s", c.err, resp.StatusCode, body)
		}
	}
}

func TestGatewayRequiresToken(t *testing.T) {
	ts := gatewayFor(t, mockBackend{})
	if resp, _ := post(t, ts, "", `{"model":"m","messages":[]}`); resp.StatusCode != 401 {
		t.Errorf("no token: status %d, want 401", resp.StatusCode)
	}
	if resp, _ := post(t, ts, "wrong", `{"model":"m","messages":[]}`); resp.StatusCode != 401 {
		t.Errorf("wrong token: status %d, want 401", resp.StatusCode)
	}
	health, err := http.Get(ts.URL + "/health")
	if err != nil || health.StatusCode != 200 {
		t.Errorf("/health should be open: %v %v", err, health)
	}
}

func TestToolResultContentArray(t *testing.T) {
	req, err := parseRequest([]byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b := req.Messages[0].Blocks[0]
	if b.Type != "tool_result" || b.ToolUseID != "t1" || b.Content != "line1\n\nline2" {
		t.Errorf("tool_result parsed as %+v", b)
	}
}

// ---- mock backend ----------------------------------------------------------

func TestMockToolLoop(t *testing.T) {
	ts := gatewayFor(t, mockBackend{})
	_, first := post(t, ts, "tok", `{"model":"m","max_tokens":50,
		"tools":[{"name":"Bash","description":"d","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"please [mock:tool] now"}]}`)
	if !strings.Contains(first, `"tool_use"`) || !strings.Contains(first, `"name":"Bash"`) || !strings.Contains(first, "mock-tool-ran") {
		t.Fatalf("mock did not call Bash: %s", first)
	}
	_, second := post(t, ts, "tok", `{"model":"m","max_tokens":50,
		"messages":[{"role":"user","content":"please [mock:tool] now"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_x","name":"Bash","input":{"command":"echo mock-tool-ran"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_x","content":"mock-tool-ran"}]}]}`)
	if !strings.Contains(second, "MOCK: tool result received: mock-tool-ran") {
		t.Errorf("mock did not answer the tool result: %s", second)
	}
}

func TestMockAnswersTheUserTurnNotTrailingSystemMessages(t *testing.T) {
	ts := gatewayFor(t, mockBackend{})
	_, body := post(t, ts, "tok", `{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"},{"type":"text","text":"hello there"}]},
		{"role":"system","content":[{"type":"text","text":"# Environment\ncwd: /tmp"}]}]}`)
	if !strings.Contains(body, "MOCK: hello there") || strings.Contains(body, "Environment") {
		t.Errorf("mock answered the wrong turn: %s", body)
	}
}

func TestMockErrorTrigger(t *testing.T) {
	ts := gatewayFor(t, mockBackend{})
	resp, body := post(t, ts, "tok", `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"[mock:error]"}]}`)
	if resp.StatusCode != 400 || !strings.Contains(body, "invalid_request_error") {
		t.Errorf("status %d: %s", resp.StatusCode, body)
	}
}

// ---- codex backend: prompt and answer parsing ------------------------------

var bashTool = toolDef{Name: "Bash", Description: "run a shell command",
	Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}

func TestCodexPromptCarriesSystemToolsAndConversation(t *testing.T) {
	p := buildCodexPrompt(chatRequest{
		System: "You are Claude Code.",
		Tools:  []toolDef{bashTool},
		Messages: []message{
			{Role: "user", Blocks: []block{{Type: "text", Text: "list files"}}},
			{Role: "assistant", Blocks: []block{{Type: "tool_use", ID: "t1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}}},
			{Role: "user", Blocks: []block{{Type: "tool_result", ToolUseID: "t1", Content: "a.txt"}}},
			{Role: "system", Blocks: []block{{Type: "text", Text: "# Environment"}}},
		},
	})
	for _, want := range []string{
		"You cannot execute anything yourself; the harness executes tools for you.",
		"request it by writing a tool_call block",
		"[Reminder: to use a tool, write a ```tool_call block",
		"=== SYSTEM INSTRUCTIONS ===\nYou are Claude Code.",
		"=== TOOLS ===", "```tool_call", "- Bash: run a shell command", `"command"`,
		"Human: list files",
		"Assistant: ```tool_call\n{\"name\": \"Bash\", \"input\": {\"command\":\"ls\"}}\n```",
		"[Tool result for call t1]\na.txt",
		"[system note]\n# Environment",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if !strings.HasSuffix(p, "Assistant:") {
		t.Errorf("prompt should end by handing the turn to the Assistant:\n%s", p)
	}
}

func TestCodexPromptHonoursToolChoice(t *testing.T) {
	none := buildCodexPrompt(chatRequest{Tools: []toolDef{bashTool}, ToolChoice: "none"})
	if strings.Contains(none, "=== TOOLS ===") || strings.Contains(none, "tool_call") {
		t.Error("tool_choice none must not describe or invite tools")
	}
	if p := buildCodexPrompt(chatRequest{Tools: []toolDef{bashTool}, ToolChoice: "any"}); !strings.Contains(p, "You must call a tool") {
		t.Error("tool_choice any should require a call")
	}
	if p := buildCodexPrompt(chatRequest{Tools: []toolDef{bashTool}, ToolChoice: "tool:Bash"}); !strings.Contains(p, "You must call the tool Bash") {
		t.Error("tool_choice tool:Bash should require that tool")
	}
}

func TestParseCodexOutput(t *testing.T) {
	tools := []toolDef{bashTool, {Name: "Read"}}
	fence := "```"
	cases := []struct {
		name      string
		out       string
		wantStop  string
		wantTypes string // comma-joined block types
		check     func(t *testing.T, blocks []block)
	}{
		{"plain text", "all done", "end_turn", "text", nil},
		{"one call", fence + "tool_call\n{\"name\":\"Bash\",\"input\":{\"command\":\"ls\"}}\n" + fence, "tool_use", "tool_use",
			func(t *testing.T, b []block) {
				if b[0].Name != "Bash" || string(b[0].Input) != `{"command":"ls"}` || !strings.HasPrefix(b[0].ID, "toolu_") {
					t.Errorf("call parsed wrong: %+v", b[0])
				}
			}},
		{"text then call", "Let me look.\n" + fence + "tool_call\n{\"name\":\"Read\",\"input\":{\"file_path\":\"/x\"}}\n" + fence, "tool_use", "text,tool_use",
			func(t *testing.T, b []block) {
				if b[0].Text != "Let me look." {
					t.Errorf("text = %q", b[0].Text)
				}
			}},
		{"parallel calls", fence + "tool_call\n{\"name\":\"Bash\",\"input\":{\"command\":\"a\"}}\n" + fence + "\n" +
			fence + "tool_call\n{\"name\":\"Read\",\"input\":{}}\n" + fence, "tool_use", "tool_use,tool_use", nil},
		{"unknown tool stays text", fence + "tool_call\n{\"name\":\"Nope\",\"input\":{}}\n" + fence, "end_turn", "text", nil},
		{"invalid JSON stays text", fence + "tool_call\n{not json}\n" + fence, "end_turn", "text", nil},
		{"input must be an object", fence + "tool_call\n{\"name\":\"Bash\",\"input\":\"ls\"}\n" + fence, "end_turn", "text", nil},
		{"json fence with name+input counts", fence + "json\n{\"name\":\"Bash\",\"input\":{\"command\":\"ls\"}}\n" + fence, "tool_use", "tool_use", nil},
		{"json fence without input is just an example", fence + "json\n{\"name\":\"Bash\"}\n" + fence, "end_turn", "text", nil},
		{"null input becomes an empty object", fence + "tool_call\n{\"name\":\"Read\",\"input\":null}\n" + fence, "tool_use", "tool_use",
			func(t *testing.T, b []block) {
				if string(b[0].Input) != `{}` {
					t.Errorf("input = %s", b[0].Input)
				}
			}},
	}
	for _, c := range cases {
		blocks, stop := parseCodexOutput(c.out, tools)
		var types []string
		for _, b := range blocks {
			types = append(types, b.Type)
		}
		if stop != c.wantStop || strings.Join(types, ",") != c.wantTypes {
			t.Errorf("%s: stop=%s types=%v, want %s / %s", c.name, stop, types, c.wantStop, c.wantTypes)
			continue
		}
		if c.check != nil {
			c.check(t, blocks)
		}
	}
}

// ---- codex backend: running the process ------------------------------------

// fakeCodex writes a stand-in `codex` that records its args, stdin and whether
// OPENAI_API_KEY reached it, then answers (or fails) as the test directs.
func fakeCodex(t *testing.T) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := `#!/bin/sh
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
printf '%s\n' "$@" > "$FAKE_CODEX_DIR/args.txt"
cat > "$FAKE_CODEX_DIR/stdin.txt"
env | grep -c '^OPENAI_API_KEY=' > "$FAKE_CODEX_DIR/keycount.txt"
echo x >> "$FAKE_CODEX_DIR/calls.txt"
if [ -n "$FAKE_CODEX_MODEL" ]; then printf 'OpenAI Codex vX\n--------\nmodel: %s\nprovider: openai\n--------\n' "$FAKE_CODEX_MODEL" >&2; fi
if [ -n "$FAKE_CODEX_FAIL" ]; then echo "$FAKE_CODEX_FAIL" >&2; exit 1; fi
printf '%s' "$FAKE_CODEX_ANSWER" > "$out"
`
	bin = filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CODEX_DIR", dir)
	return bin, dir
}

func TestCodexRunPassesTheRightFlagsAndPromptAndHidesTheAPIKey(t *testing.T) {
	bin, dir := fakeCodex(t)
	t.Setenv("FAKE_CODEX_ANSWER", "the answer")
	t.Setenv("OPENAI_API_KEY", "sk-must-not-reach-codex")
	cb := &codexBackend{bin: bin, workdir: t.TempDir(), model: "gpt-x"}

	res, err := cb.Complete(context.Background(), chatRequest{
		Messages: []message{{Role: "user", Blocks: []block{{Type: "text", Text: "ping"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Blocks) != 1 || res.Blocks[0].Text != "the answer" || res.StopReason != "end_turn" {
		t.Errorf("result = %+v", res)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(args) < 3 || args[0] != "--no-daemon" || args[1] != "exec" || args[len(args)-1] != "-" {
		t.Fatalf("args should start --no-daemon exec and end with -: %v", args)
	}
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{" --ephemeral ", " --ignore-user-config ", " --sandbox read-only ", " --skip-git-repo-check ",
		" --disable shell_tool ", " --disable unified_exec ", " --disable code_mode_host ", " -m gpt-x "} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q: %v", want, args)
		}
	}

	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin.txt"))
	if !strings.Contains(string(stdin), "Human: ping") {
		t.Errorf("prompt did not arrive on stdin: %q", stdin)
	}
	if kc, _ := os.ReadFile(filepath.Join(dir, "keycount.txt")); strings.TrimSpace(string(kc)) != "0" {
		t.Errorf("OPENAI_API_KEY leaked into codex's environment (count %s)", kc)
	}
}

func TestCodexRunWithoutModelOmitsTheFlag(t *testing.T) {
	bin, dir := fakeCodex(t)
	t.Setenv("FAKE_CODEX_ANSWER", "x")
	cb := &codexBackend{bin: bin, workdir: t.TempDir()}
	if _, err := cb.Complete(context.Background(), chatRequest{Messages: []message{{Role: "user", Blocks: []block{{Type: "text", Text: "q"}}}}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	if strings.Contains(" "+strings.ReplaceAll(string(raw), "\n", " ")+" ", " -m ") {
		t.Errorf("-m must be absent when no model was chosen: %s", raw)
	}
}

func TestCodexFailuresMapToAnthropicErrors(t *testing.T) {
	cases := []struct {
		stderr string
		status int
		typ    string
	}{
		{"ERROR: 401 Unauthorized", 401, "authentication_error"},
		{"You have hit your usage limit", 429, "rate_limit_error"},
		{"segfault or something", 502, "api_error"},
	}
	for _, c := range cases {
		bin, _ := fakeCodex(t)
		t.Setenv("FAKE_CODEX_FAIL", c.stderr)
		cb := &codexBackend{bin: bin, workdir: t.TempDir()}
		_, err := cb.Complete(context.Background(), chatRequest{Messages: []message{{Role: "user", Blocks: []block{{Type: "text", Text: "q"}}}}})
		var berr *backendError
		if !errors.As(err, &berr) || berr.Status != c.status || berr.Type != c.typ {
			t.Errorf("%q: err = %v, want %d %s", c.stderr, err, c.status, c.typ)
		}
	}
}

func TestCodexEmptyAnswerIsAnError(t *testing.T) {
	bin, _ := fakeCodex(t)
	t.Setenv("FAKE_CODEX_ANSWER", "   ")
	cb := &codexBackend{bin: bin, workdir: t.TempDir()}
	_, err := cb.Complete(context.Background(), chatRequest{Messages: []message{{Role: "user", Blocks: []block{{Type: "text", Text: "q"}}}}})
	var berr *backendError
	if !errors.As(err, &berr) || berr.Status != 502 || !strings.Contains(berr.Message, "empty") {
		t.Errorf("err = %v", err)
	}
}

func TestCodexToolCallComesBackAsToolUse(t *testing.T) {
	bin, _ := fakeCodex(t)
	t.Setenv("FAKE_CODEX_ANSWER", "Running it.\n```tool_call\n{\"name\":\"Bash\",\"input\":{\"command\":\"echo hi\"}}\n```")
	cb := &codexBackend{bin: bin, workdir: t.TempDir()}
	res, err := cb.Complete(context.Background(), chatRequest{
		Tools:    []toolDef{bashTool},
		Messages: []message{{Role: "user", Blocks: []block{{Type: "text", Text: "run echo hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "tool_use" || len(res.Blocks) != 2 || res.Blocks[0].Text != "Running it." ||
		res.Blocks[1].Name != "Bash" || string(res.Blocks[1].Input) != `{"command":"echo hi"}` {
		t.Errorf("result = %+v", res)
	}
}

func TestSnapCodexIsNeverUsed(t *testing.T) {
	if !isSnapPath("/snap/bin/codex") {
		t.Error("the unofficial snap build must be recognised and skipped")
	}
	if isSnapPath("/home/u/.vscode/extensions/openai.chatgpt-1/bin/linux-x86_64/codex") {
		t.Error("the extension-bundled binary must not be mistaken for the snap")
	}
}

// ---- codex backend: model detection ----------------------------------------

func callCount(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "calls.txt"))
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "x")
}

func TestResolveModelReadsTheHeaderPinsItAndCachesIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the cache lives under ~/.codex-claude
	bin, dir := fakeCodex(t)
	t.Setenv("FAKE_CODEX_ANSWER", "ok")
	t.Setenv("FAKE_CODEX_MODEL", "gpt-test-1")

	cb := &codexBackend{bin: bin, workdir: t.TempDir()}
	got, err := cb.ResolveModel(context.Background())
	if err != nil || got != "gpt-test-1" {
		t.Fatalf("ResolveModel = %q, %v", got, err)
	}
	if cb.model != "gpt-test-1" {
		t.Errorf("model must be pinned, got %q", cb.model)
	}
	if n := callCount(t, dir); n != 1 {
		t.Fatalf("detection should cost exactly one codex call, got %d", n)
	}

	// Real requests now carry the pinned model, so the label is the truth.
	if _, err := cb.Complete(context.Background(), chatRequest{Messages: []message{{Role: "user", Blocks: []block{{Type: "text", Text: "q"}}}}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	if !strings.Contains(" "+strings.ReplaceAll(string(raw), "\n", " ")+" ", " -m gpt-test-1 ") {
		t.Errorf("pinned model not passed to codex: %s", raw)
	}

	// A fresh backend reuses the cache: no further detection call.
	before := callCount(t, dir)
	cb2 := &codexBackend{bin: bin, workdir: t.TempDir()}
	if got, _ := cb2.ResolveModel(context.Background()); got != "gpt-test-1" {
		t.Errorf("cached ResolveModel = %q", got)
	}
	if after := callCount(t, dir); after != before {
		t.Errorf("cache was ignored: %d -> %d codex calls", before, after)
	}
}

func TestResolveModelCacheIsKeyedByBinaryAndExpires(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin, _ := fakeCodex(t)
	writeModelCache("/some/other/codex", "stale-model")
	if got := readModelCache(bin); got != "" {
		t.Errorf("a cache written for another binary must not be used, got %q", got)
	}
	writeModelCache(bin, "fresh-model")
	if got := readModelCache(bin); got != "fresh-model" {
		t.Errorf("cache read = %q", got)
	}
	old, _ := json.Marshal(modelCache{Bin: bin, Model: "old-model", At: time.Now().Add(-13 * time.Hour).Unix()})
	_ = os.WriteFile(modelCachePath(), old, 0o600)
	if got := readModelCache(bin); got != "" {
		t.Errorf("a cache older than 12h must be ignored, got %q", got)
	}
}

func TestResolveModelHonoursAnExplicitModelWithoutCallingCodex(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin, dir := fakeCodex(t)
	cb := &codexBackend{bin: bin, workdir: t.TempDir(), model: "chosen-by-user"}
	if got, err := cb.ResolveModel(context.Background()); err != nil || got != "chosen-by-user" {
		t.Errorf("ResolveModel = %q, %v", got, err)
	}
	if n := callCount(t, dir); n != 0 {
		t.Errorf("an explicit model needs no detection call, got %d", n)
	}
}

func TestResolveModelWithoutAHeaderReturnsEmptyNotAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin, _ := fakeCodex(t)
	t.Setenv("FAKE_CODEX_ANSWER", "ok") // no FAKE_CODEX_MODEL: no header printed
	cb := &codexBackend{bin: bin, workdir: t.TempDir()}
	if got, err := cb.ResolveModel(context.Background()); err != nil || got != "" {
		t.Errorf("ResolveModel = %q, %v; want empty, nil", got, err)
	}
}

func TestResolveModelSurfacesCodexFailures(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin, _ := fakeCodex(t)
	t.Setenv("FAKE_CODEX_FAIL", "ERROR: 401 Unauthorized")
	cb := &codexBackend{bin: bin, workdir: t.TempDir()}
	_, err := cb.ResolveModel(context.Background())
	var berr *backendError
	if !errors.As(err, &berr) || berr.Status != 401 {
		t.Errorf("err = %v, want a 401 backendError", err)
	}
}
