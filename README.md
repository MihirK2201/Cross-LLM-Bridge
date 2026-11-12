# Cross-LLM-Bridge

Two small Linux tools that run each coding agent's harness on the other vendor's model, using your own logins and no API keys.

| Tool | Harness | Model | Uses your |
|---|---|---|---|
| [`claude-codex`](claude-codex/) | OpenAI Codex | Claude | Claude Code login |
| [`codex-claude`](codex-claude/) | Claude Code | GPT | Codex (ChatGPT) login |

## How it works

Each tool is a single Go binary. It starts a small gateway on `127.0.0.1` and launches the real harness pointed at it. For every request the gateway runs the other vendor's official CLI as a plain model (`claude -p` or `codex exec`) and translates the answer back, tool calls included. The harness keeps running its own tools, permissions and UI. No login token is read or copied by these tools.

Both ask for consent on first run, say exactly what they do and where your data goes, and run the harness in an isolated or per-call configuration.

## Quick start

```bash
cd claude-codex && ./install.sh     # Codex harness on Claude
cd codex-claude && ./install.sh     # Claude Code harness on GPT
```

Each folder has its own README with usage, flags and details.

Requirements: Linux x86_64, Go to build, Claude Code (`claude`) and the Codex CLI (or the OpenAI Codex VS Code extension), each logged in.

## Status and known limitations

Early (v0). Developed and tested on one machine: Ubuntu 22.04, Claude Code 2.1.318, Codex 0.162.0-alpha.

- **Codex versions.** Both tools pass Codex flags such as `--no-daemon`, `--ignore-user-config` and `--ephemeral`, and `claude-codex` relies on Codex's current tool-call format. I have only checked these against the Codex bundled in the VS Code extension. Other builds (for example the unofficial snap package) can fail.
- **`claude-codex`.** Its login check only verifies that `~/.claude` exists, its local bridge has no per-session token (`codex-claude` has one), and it has no automated tests.
- **`codex-claude`.** Tool use is prompt-emulated rather than native function calling, so it is slower and may occasionally answer in text instead of requesting a tool.
- Linux only.

## Terms

These tools route a personal Claude or ChatGPT plan through a different harness. That is a gray area under both vendors' terms. They are meant for personal research and harness testing on your own machine and accounts, not for shared or production use. Check the current terms of both vendors before relying on them.

This project is not affiliated with or endorsed by Anthropic or OpenAI. Claude, Claude Code, Codex, ChatGPT and GPT are trademarks of their respective owners.
