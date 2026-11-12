#!/usr/bin/env bash
# install.sh — install the `claude-codex` binary into ~/.local/bin for the
# current user. No root, nothing system-wide. Run from a clone of this repo:
#   ./install.sh
# or pipe a hosted copy:  curl -fsSL <url>/install.sh | bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_SRC="$REPO_DIR/claude-codex"
DEST_DIR="$HOME/.local/bin"
DEST="$DEST_DIR/claude-codex"

echo "claude-codex installer"
echo

# Prefer a prebuilt binary in the repo; otherwise build it if Go is available.
if [ ! -x "$BIN_SRC" ]; then
  if command -v go >/dev/null 2>&1; then
    echo "No prebuilt binary found — building from source with go…"
    ( cd "$REPO_DIR" && go build -o claude-codex . )
  else
    echo "ERROR: no prebuilt ./claude-codex binary and Go is not installed to build it." >&2
    echo "Install Go, or obtain a prebuilt claude-codex binary, then re-run." >&2
    exit 1
  fi
fi

mkdir -p "$DEST_DIR"
install -m 0755 "$BIN_SRC" "$DEST"
echo "Installed: $DEST"

# PATH check — purely informational, we do not edit the user's shell files here.
case ":$PATH:" in
  *":$DEST_DIR:"*) ;;
  *)
    echo
    echo "NOTE: $DEST_DIR is not on your PATH. Add this line to ~/.bashrc:"
    echo "    export PATH=\"\$HOME/.local/bin:\$PATH\""
    ;;
esac

echo
echo "Done. Next steps:"
echo "  1. Make sure you are logged in to Claude Code:  claude"
echo "  2. Run Codex on your Claude login:              claude-codex \"say hello\""
echo
echo "On first run it will explain exactly what it does and ask for your consent."
echo "Preview without changing anything:  claude-codex --dry-run"
