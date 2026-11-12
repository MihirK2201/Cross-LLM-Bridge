# claude-codex

Run OpenAI **Codex** on your own **Claude Code** login — no API key, no shared server. One binary, one command.

Each developer uses their *own* `~/.claude` session on their *own* machine, so there is no shared account and no shared rate limit.

## What it is

`claude-codex` is a single Go binary that:

1. Starts a local OpenAI-compatible bridge **in-process** on `127.0.0.1` (a random free port) for the lifetime of one command — it stops the moment the command exits.
2. Launches the real **Codex CLI** pointed at that bridge entirely via `-c` flags, so your `~/.codex/config.toml` is **never edited**.
3. The bridge translates Codex's OpenAI-style requests (including the Responses API and Codex's `custom_tool_call` / namespaced tools) into calls to your local `claude` CLI, which uses your existing login.

No token is copied out of `~/.claude`. Nothing is sent to any external service beyond what Claude Code itself already does.

## Install

From a clone of this repo:

```bash
./install.sh
```

This copies the `claude-codex` binary to `~/.local/bin`. No root, nothing system-wide. If `~/.local/bin` isn't on your `PATH`, the installer tells you the one line to add.

## Use

```bash
claude-codex "fix the failing tests"     # one-shot task
claude-codex                             # interactive session
claude-codex exec "summarize README.md"  # explicit Codex subcommand
```

Any arguments after `claude-codex` are passed straight through to Codex.

### Choosing the Claude model

```bash
claude-codex -m opus "design the schema"      # Opus
claude-codex --model haiku "quick rename"     # Haiku
claude-codex "normal task"                    # Sonnet (default)
claude-codex -m claude-opus-4-5 "…"           # a full model id, honoured exactly
```

`opus | sonnet | haiku` pick the tier; a full `claude-*` id is passed through to
the `claude` CLI unchanged. Anything unrecognized falls back to the default (`sonnet`).

**Interactive menu (default).** On a real terminal, `claude-codex` shows the model menu automatically after the bridge starts — you don't need any flag:

```
  Select a Claude model  ↑/↓ · Enter · q to keep default
  ❯ Sonnet  balanced · default
    Opus    most capable
    Haiku   fastest
```

Navigate with ↑/↓ (or `j`/`k`), Enter to choose, `1`/`2`/`3` to jump, `q`/Esc to keep the default.

The menu is **skipped** (and the flag value or default used) when you pin a model with `-m`, pass `--quiet`, or run non-interactively (piped/CI). Force it anytime with `--pick`.

## Prerequisites

- **Claude Code** installed and logged in (`claude` on your PATH; run `claude` once to log in).
- **Codex CLI** — if it isn't installed, `claude-codex` asks (separately, defaulting to *no*) whether to run `npm install -g @openai/codex`. It never installs anything system-wide without an explicit yes.

## Consent & transparency

Nothing is written or installed before you agree.

- **First run** prints exactly what it does and waits for `y`. Your choice is remembered in `~/.claude-codex/consent` so it asks only once.
- Installing the Codex CLI is a **separate** explicit prompt that defaults to no.

Flags:

| Flag | What it does |
|---|---|
| `--dry-run` | Print exactly what would happen; change nothing. |
| `--what` | Show current state (claude login, codex, consent). |
| `--yes` | Pre-approve the first-run consent (for a reviewed/managed rollout). |
| `--model` / `-m` | Pin the Claude model (skips the menu): `opus` \| `sonnet` \| `haiku`, or a full `claude-*` id. |
| `--pick` | Force the model menu (it already shows by default on a terminal). |
| `--demo` | Play the animated startup sequence and exit (no work, no Codex). |
| `--quiet` | No banner, colour, or animation (also auto-off when piped). |
| `--uninstall` | Remove the consent marker and print cleanup steps. |
| `--version` | Print the version. |

Set `CLAUDE_CODEX_DEBUG=1` to see the bridge's per-request logs.

## The experience

On a real terminal you get a branded banner, an animated startup (braille spinners that
resolve to green ✓ steps for *Claude login found → Codex ready → Bridge ready*), and a
`▸ Launching Codex` handoff before Codex takes over. It all degrades to clean plain text
automatically when output is piped, when `NO_COLOR` is set, or with `--quiet`, so scripts
and CI stay unaffected.

## Uninstall

```bash
claude-codex --uninstall      # removes the consent marker, prints steps
rm ~/.local/bin/claude-codex  # removes the binary
```

`claude-codex` never edits `~/.codex/config.toml`, so there is nothing there to revert.

## A note for the organization

This routes each person's Claude **subscription** through a local wrapper. That is a gray area under Anthropic's consumer terms, which have at times restricted subscription use outside Claude Code. Keeping it strictly per-user / own-account keeps the risk low, but before adopting this as official company tooling, have someone sign off against Anthropic's current terms. For production or client-facing workloads, use the Anthropic API with proper billing instead.
