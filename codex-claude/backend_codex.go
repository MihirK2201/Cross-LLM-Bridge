// backend_codex.go — the gateway's model backend: the official Codex CLI
// (`codex exec`) running under the user's own ChatGPT login. It is the mirror
// of claude-codex, which spawns the official `claude` CLI. This tool never
// reads or copies the login: codex uses its own ~/.codex exactly as it does when
// you run it yourself.
//
// Codex is itself an agent, so it is run as a plain model: its own tools are
// switched off, it runs in a read-only sandbox inside an empty directory, and
// the harness's tools (Claude Code's) are described in the prompt. The model
// asks for one by writing a fenced ```tool_call block, which is parsed back
// into a real tool_use for Claude Code to run.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// codexDisabledFeatures are Codex's own tool surfaces, switched off so it
// behaves as a model rather than a second agent.
var codexDisabledFeatures = []string{
	"shell_tool", "unified_exec", "code_mode_host", "view_image", "image_generation",
	"plugins", "apps", "browser_use", "computer_use", "hooks", "skill_search",
	"sleep_tool", "tool_suggest",
}

type codexBackend struct {
	bin     string // the official codex binary
	workdir string // empty directory codex runs in, so no project files leak in
	model   string // "" = the account's default model
}

func isSnapPath(p string) bool {
	if strings.Contains(p, "/snap/") {
		return true
	}
	if target, err := filepath.EvalSymlinks(p); err == nil && strings.Contains(target, "/snap/") {
		return true
	}
	return false
}

// findCodexBinary prefers the official binary bundled with the VS Code Codex
// extension, then a `codex` on PATH. It never returns the unofficial snap build.
func findCodexBinary() string {
	matches, _ := filepath.Glob(filepath.Join(homeDir(), ".vscode/extensions/openai.chatgpt-*/bin/linux-x86_64/codex"))
	sort.Strings(matches)
	if len(matches) > 0 {
		return matches[len(matches)-1]
	}
	if p, err := exec.LookPath("codex"); err == nil && !isSnapPath(p) {
		return p
	}
	return ""
}

func newCodexBackend(model string) (*codexBackend, error) {
	bin := findCodexBinary()
	if bin == "" {
		return nil, errors.New("the official Codex CLI was not found.\n" +
			"  Install the OpenAI Codex extension or CLI and run `codex login`, then try again.")
	}
	work := filepath.Join(stateDir(), "codex-work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	return &codexBackend{bin: bin, workdir: work, model: model}, nil
}

func (c *codexBackend) Name() string { return "codex" }

// Preflight confirms codex is logged in.
func (c *codexBackend) Preflight(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, c.bin, "login", "status").CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil || !strings.Contains(strings.ToLower(text), "logged in") {
		return "", errors.New("you are not logged in to Codex. Run `codex login`, then try again.")
	}
	return text + " (Codex)", nil
}

// ---- prompt ----------------------------------------------------------------

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func renderMessage(m message) string {
	var parts []string
	for _, b := range m.Blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "tool_use":
			parts = append(parts, "```tool_call\n{\"name\": "+jsonString(b.Name)+", \"input\": "+string(nonEmptyJSON(b.Input))+"}\n```")
		case "tool_result":
			label := "Tool result"
			if b.IsError {
				label = "Tool error"
			}
			parts = append(parts, fmt.Sprintf("[%s for call %s]\n%s", label, b.ToolUseID, b.Content))
		}
	}
	body := strings.Join(parts, "\n\n")
	switch m.Role {
	case "assistant":
		return "Assistant: " + body + "\n\n"
	case "system":
		return "[system note]\n" + body + "\n\n"
	default:
		return "Human: " + body + "\n\n"
	}
}

func codexToolInstructions(req chatRequest) string {
	var b strings.Builder
	b.WriteString("=== TOOLS ===\n")
	b.WriteString("The harness offers the tools listed below. To call one, write a fenced block exactly like this " +
		"(write several blocks to call tools in parallel):\n\n")
	b.WriteString("```tool_call\n{\"name\": \"<tool name>\", \"input\": { ...arguments... }}\n```\n\n")
	b.WriteString("\"input\" must satisfy the tool's input schema. After a tool runs, its output appears in the " +
		"conversation as \"Tool result\". If no tool is needed, answer in plain text with no tool_call block.\n")
	switch {
	case req.ToolChoice == "any":
		b.WriteString("You must call a tool in this turn.\n")
	case strings.HasPrefix(req.ToolChoice, "tool:"):
		b.WriteString("You must call the tool " + strings.TrimPrefix(req.ToolChoice, "tool:") + " in this turn.\n")
	}
	b.WriteString("\nTools:\n")
	for _, t := range req.Tools {
		b.WriteString("- " + t.Name + ": " + strings.TrimSpace(t.Description) + "\n")
		if len(t.Schema) > 0 {
			b.WriteString("  input_schema: " + strings.TrimSpace(string(t.Schema)) + "\n")
		}
	}
	return b.String()
}

func buildCodexPrompt(req chatRequest) string {
	var b strings.Builder
	withTools := len(req.Tools) > 0 && req.ToolChoice != "none"
	b.WriteString("You are the language model behind an API, answering for a coding-assistant harness. " +
		"Write the next Assistant turn of the conversation below. " +
		"You cannot execute anything yourself; the harness executes tools for you.")
	if withTools {
		b.WriteString(" When the task needs a tool, request it by writing a tool_call block " +
			"(format under TOOLS) and the harness will run it and show you the result. " +
			"Do not say you cannot use tools: requesting them this way is how you use them.")
	}
	b.WriteString("\n\n")
	if req.System != "" {
		b.WriteString("=== SYSTEM INSTRUCTIONS ===\n" + req.System + "\n\n")
	}
	if withTools {
		b.WriteString(codexToolInstructions(req) + "\n")
	}
	b.WriteString("=== CONVERSATION ===\n")
	for _, m := range req.Messages {
		b.WriteString(renderMessage(m))
	}
	if withTools {
		b.WriteString("[Reminder: to use a tool, write a ```tool_call block as described under TOOLS; " +
			"otherwise answer in plain text.]\n\n")
	}
	b.WriteString("Assistant:")
	return b.String()
}

// ---- answer parsing --------------------------------------------------------

var toolFenceRe = regexp.MustCompile("(?s)```(tool_call|json)?[ \\t]*\\r?\\n(.*?)```")

// parseToolCall accepts a fenced body as a tool call only if it names a tool the
// harness offered and its input is a JSON object. A bare ```json block must also
// carry an "input"/"arguments" key, so ordinary JSON examples in an answer are
// not mistaken for calls.
func parseToolCall(body string, explicit bool, known map[string]bool) (block, bool) {
	var c struct {
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &c) != nil || !known[c.Name] {
		return block{}, false
	}
	in := c.Input
	if len(in) == 0 {
		in = c.Arguments
	}
	if len(in) == 0 && !explicit {
		return block{}, false
	}
	input := json.RawMessage("{}")
	if len(in) > 0 {
		var obj map[string]any
		if json.Unmarshal(in, &obj) != nil {
			return block{}, false
		}
		if obj != nil {
			input = in
		}
	}
	return block{Type: "tool_use", ID: "toolu_" + randHex(12), Name: c.Name, Input: input}, true
}

// parseCodexOutput splits the model's answer into text and tool_use blocks.
func parseCodexOutput(out string, tools []toolDef) ([]block, string) {
	known := map[string]bool{}
	for _, t := range tools {
		known[t.Name] = true
	}
	var text strings.Builder
	var calls []block
	last := 0
	for _, m := range toolFenceRe.FindAllStringSubmatchIndex(out, -1) {
		lang := ""
		if m[2] >= 0 {
			lang = out[m[2]:m[3]]
		}
		call, ok := parseToolCall(out[m[4]:m[5]], lang == "tool_call", known)
		if !ok {
			continue // not a call: stays in the text
		}
		text.WriteString(out[last:m[0]])
		last = m[1]
		calls = append(calls, call)
	}
	text.WriteString(out[last:])

	var blocks []block
	if t := strings.TrimSpace(text.String()); t != "" {
		blocks = append(blocks, block{Type: "text", Text: t})
	}
	blocks = append(blocks, calls...)
	if len(calls) > 0 {
		return blocks, "tool_use"
	}
	return blocks, "end_turn"
}

// ---- running codex ---------------------------------------------------------

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func codexError(stderr string) *backendError {
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "401") || strings.Contains(low, "unauthorized") || strings.Contains(low, "not logged in"):
		return &backendError{Status: 401, Type: "authentication_error",
			Message: "Codex login problem. Run `codex login`. Details: " + truncate(lastLines(stderr, 4), 400)}
	case strings.Contains(low, "429") || strings.Contains(low, "rate limit") || strings.Contains(low, "usage limit"):
		return &backendError{Status: 429, Type: "rate_limit_error",
			Message: "Codex usage or rate limit reached. Details: " + truncate(lastLines(stderr, 4), 400)}
	}
	return &backendError{Status: 502, Type: "api_error",
		Message: "Codex CLI failed: " + truncate(lastLines(stderr, 6), 600)}
}

// invoke runs codex once. It returns the model's answer (read from the
// --output-last-message file) and codex's combined output, whose session header
// names the model that actually answered.
func (c *codexBackend) invoke(ctx context.Context, prompt string) (answer, output string, err error) {
	outFile := filepath.Join(os.TempDir(), "codex-claude-"+randHex(8)+".txt")
	defer os.Remove(outFile)

	// --no-daemon is a global flag and must precede `exec`. --ignore-user-config
	// keeps the user's own ~/.codex/config.toml (which may point at another
	// provider) out of it; the login itself is still read by codex as usual.
	args := []string{"--no-daemon", "exec",
		"--skip-git-repo-check", "--ephemeral", "--ignore-user-config",
		"--sandbox", "read-only", "-C", c.workdir, "--color", "never",
		"--output-last-message", outFile}
	for _, f := range codexDisabledFeatures {
		args = append(args, "--disable", f)
	}
	if c.model != "" {
		args = append(args, "-m", c.model)
	}
	args = append(args, "-") // read the prompt from stdin (it can exceed argv limits)

	cmd := exec.CommandContext(ctx, c.bin, args...)
	// Use the ChatGPT login: an API key in the environment must not take over.
	cmd.Env = filterEnv(os.Environ(), "OPENAI_API_KEY", "CODEX_API_KEY")
	cmd.Stdin = strings.NewReader(prompt)
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	runErr := cmd.Run()

	if b, rerr := os.ReadFile(outFile); rerr == nil {
		if text := strings.TrimSpace(string(b)); text != "" {
			return text, log.String(), nil
		}
	}
	if ctx.Err() != nil {
		return "", log.String(), ctx.Err()
	}
	if runErr == nil {
		return "", log.String(), &backendError{Status: 502, Type: "api_error", Message: "Codex returned an empty answer"}
	}
	return "", log.String(), codexError(log.String())
}

func (c *codexBackend) Complete(ctx context.Context, req chatRequest) (chatResult, error) {
	prompt := buildCodexPrompt(req)
	out, _, err := c.invoke(ctx, prompt)
	if err != nil {
		return chatResult{}, err
	}
	tools := req.Tools
	if req.ToolChoice == "none" {
		tools = nil
	}
	blocks, stop := parseCodexOutput(out, tools)
	return chatResult{
		Blocks:       blocks,
		StopReason:   stop,
		InputTokens:  roughTokens(prompt),
		OutputTokens: roughTokens(out),
	}, nil
}

// ---- model name ------------------------------------------------------------

var modelHeaderRe = regexp.MustCompile(`(?m)^model:\s*(\S+)`)

const modelCacheTTL = 12 * time.Hour

type modelCache struct {
	Bin   string `json:"bin"`
	Model string `json:"model"`
	At    int64  `json:"at"`
}

func modelCachePath() string { return filepath.Join(stateDir(), "model-cache.json") }

func readModelCache(bin string) string {
	b, err := os.ReadFile(modelCachePath())
	if err != nil {
		return ""
	}
	var mc modelCache
	if json.Unmarshal(b, &mc) != nil || mc.Bin != bin || mc.Model == "" {
		return ""
	}
	if time.Since(time.Unix(mc.At, 0)) > modelCacheTTL {
		return ""
	}
	return mc.Model
}

func writeModelCache(bin, model string) {
	_ = os.MkdirAll(stateDir(), 0o700)
	b, _ := json.Marshal(modelCache{Bin: bin, Model: model, At: time.Now().Unix()})
	_ = os.WriteFile(modelCachePath(), b, 0o600)
}

// ResolveModel finds out which model Codex uses for this account and pins it,
// so the name shown in Claude Code is the model that really answers. The
// account default is chosen by Codex at run time, so it asks codex itself: one
// tiny request whose session header names the model, cached for 12 hours.
// It returns "" if codex answered but printed no model line.
func (c *codexBackend) ResolveModel(ctx context.Context) (string, error) {
	if c.model != "" {
		return c.model, nil
	}
	if m := readModelCache(c.bin); m != "" {
		c.model = m
		return m, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	_, out, err := c.invoke(ctx, "Reply with exactly: ok")
	if m := modelHeaderRe.FindStringSubmatch(out); m != nil {
		c.model = m[1]
		if err == nil {
			writeModelCache(c.bin, m[1])
		}
		return m[1], nil
	}
	return "", err
}
