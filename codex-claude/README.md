# codex-claude

Run the **Claude Code** harness on a **GPT model**, through your own **Codex login**. It is the mirror of `claude-codex`: that runs the Codex harness on Claude, this runs the Claude Code harness on GPT. No API key.

## How it works

`codex-claude` starts a small local gateway that speaks the Anthropic Messages API, then launches the real `claude` CLI pointed at it. For every request the gateway runs the official `codex exec` under your ChatGPT login as a plain model, and translates the answer back to Claude Code, tool calls included. Claude Code's own tools, permissions, TUI and loop run as normal; only the model behind it changes.

Codex is itself an agent, so it is run as a model: its own tools are switched off, it runs in a read-only sandbox inside an empty directory, and Claude Code's tools are described in the prompt. The model asks for one by writing a fenced `tool_call` block, which the gateway turns into a real tool call for Claude Code to run.

## Quick start

```bash
./install.sh                         # builds if needed, installs to ~/.local/bin

codex-claude                         # interactive Claude Code on GPT
codex-claude -p "explain main.go"    # one-shot (everything after our flags goes to claude)
codex-claude -m <model> -p "..."     # choose a Codex model (default: your ChatGPT account's default)
codex-claude --backend mock -p "hi"  # no model at all: test the harness itself
```

Requirements:
- Claude Code installed (`claude` on PATH). You do not need to be logged in to Claude: the session runs in its own config directory.
- The official Codex CLI or the OpenAI Codex VS Code extension, logged in with `codex login`.

## Flags

Flags before the first unrecognized argument belong to `codex-claude`; the rest are passed to `claude`. Use `--` to force the split.

| Flag | Meaning |
|---|---|
| `--backend codex\|mock` | Model backend (default `codex`). |
| `-m`, `--model NAME` | Codex model to use (default: your account's default, detected at startup). |
| `--yes`, `-y` | Pre-approve the first-run consent. |
| `--quiet` | No banner, colour or animation (also automatic when piped). |
| `--what` | Show current state (claude, codex, login, consent) and exit. |
| `--uninstall` | Remove the consent marker and print cleanup steps. |
| `--serve PORT` | Debug: run only the gateway on `127.0.0.1:PORT` and print its token. |
| `--version` | Print the version. |

Set `CODEX_CLAUDE_DEBUG=1` to log every gateway request, including a one-line summary of each message block.

## The model name

Codex picks your account's default model at run time, so `codex-claude` asks Codex once (a tiny request whose session header names the model), caches it in `~/.codex-claude/model-cache.json` for 12 hours, and pins it. Claude Code then shows the real model, and the model answers to that name. `-m NAME` skips the detection and uses the model you chose.

## What it does on your machine

On the first run it explains this and waits for a yes:

- Runs a gateway on `127.0.0.1` (random port) only while the session lasts. Every request needs a random per-session token, so other local software or web pages cannot use it.
- Sends your prompts, and the files Claude Code reads for a task, to OpenAI by running the official `codex` CLI under your ChatGPT login. It counts against your plan's Codex usage. This tool never reads or copies the login; `codex` uses it the way it always does. At startup it also makes one tiny `Reply with exactly: ok` request to learn which model your account uses, and caches the answer for 12 hours. An `OPENAI_API_KEY` in your environment is removed for the codex process so the ChatGPT login is what gets used.
- Launches `claude` with `CLAUDE_CONFIG_DIR=~/.codex-claude/claude-home`, so your real `~/.claude` (login, settings, history) is neither used nor modified. Telemetry, the auto-updater and the plugin-catalog download are switched off.
- Runs codex with `--ephemeral --ignore-user-config`, so your own `~/.codex/config.toml` is not loaded and no Codex session is saved. During development, no file under `~/.codex` changed during runs.

When checked during development, the isolated `claude` process had no non-loopback sockets, in both one-shot and interactive mode. The gateway's `codex` child is what talks to OpenAI.

The first time you use a folder, Claude Code shows its own "trust this folder?" dialog (default: *No, exit*). That is Claude Code, not this tool.

## The mock backend

`--backend mock` needs no network or login and answers deterministically, so you can test harness behaviour. Put a trigger anywhere in the prompt:

| Trigger | Effect |
|---|---|
| *(none)* | Replies `MOCK: <your prompt>`. |
| `[mock:tool]` | Requests a `Bash` call (`echo mock-tool-ran`). After the result comes back it replies `MOCK: tool result received: <output>`. |
| `[mock:error]` | Fails with a 400, to see how Claude Code surfaces API errors. |
| `[mock:slow]` | Waits about 8 seconds before answering. |

```bash
codex-claude --backend mock -p "run it [mock:tool]" --allowedTools "Bash(echo *)"
# -> MOCK: tool result received: mock-tool-ran
```

## Limitations

- **Tool use is prompt-emulated.** Claude Code's tools are described in the prompt and GPT requests them with a `tool_call` block, rather than through native function calling. In testing, a `Bash` call and a file `Read` both worked end to end; longer multi-step tasks have not been exercised, and a model may occasionally answer in text instead of requesting a tool.
- **It is slower than a native model.** Each turn starts a `codex` process and re-sends the whole conversation, including Claude Code's large system prompt: roughly 5 seconds for a plain answer, 10–20 seconds for a tool round trip.
- **Streaming is synthetic.** The answer is replayed to Claude Code as an SSE stream once complete, so text appears in one piece.
- No image input (images are replaced by a short note) and no extended-thinking blocks.
- Claude Code prints a one-time note that it does not recognise the model name (for example `gpt-6-luna`) and assumes a 200k context window. It is harmless.

## Terms

This routes a ChatGPT plan's Codex through a different harness. That is a gray area under OpenAI's terms for the plan, exactly as `claude-codex` is for a Claude plan. It is meant for personal research and harness testing on your own machine and account, not for shared or production use.

## Development

```bash
go vet ./... && go test ./...
```

Layout: `main.go` (CLI, consent, launch), `gateway.go` (Anthropic API server), `backend_codex.go` (the Codex backend: prompt, process, tool-call parsing), `backend_mock.go` (scripted model), `ui.go` (terminal UI), `types.go` (shared model). The Codex backend is tested against a fake `codex` executable that checks the exact flags, stdin and environment.
