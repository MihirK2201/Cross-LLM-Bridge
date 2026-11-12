#!/usr/bin/env bash
# install.sh — install the `codex-claude` binary into ~/.local/bin for the
# current user. No root, nothing system-wide. Run from a clone of this repo:
#   ./install.sh
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEST_DIR="$HOME/.local/bin"
DEST="$DEST_DIR/codex-claude"

echo "codex-claude installer"
echo

# Always rebuild when Go is available, so the installed binary can never lag the source.
if command -v go >/dev/null 2>&1; then
  echo "Building from source…"
  ( cd "$REPO_DIR" && go build -o codex-claude . )
elif [ ! -x "$REPO_DIR/codex-claude" ]; then
  echo "ERROR: Go is not installed and there is no prebuilt ./codex-claude binary." >&2
  exit 1
fi

mkdir -p "$DEST_DIR"
install -m 0755 "$REPO_DIR/codex-claude" "$DEST"
echo "Installed: $DEST"

case ":$PATH:" in
  *":$DEST_DIR:"*) ;;
  *)
    echo
    echo "NOTE: $DEST_DIR is not on your PATH. Add this line to ~/.bashrc:"
    echo "    export PATH=\"\$HOME/.local/bin:\$PATH\""
    ;;
esac

echo
echo "Next:"
echo "  codex login        # once, if you are not already logged in to Codex"
echo "  codex-claude"
echo
echo "On first run it explains exactly what it does and asks for your consent."
