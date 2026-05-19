#!/bin/bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
# local-install.sh — Build and install gemini-api CLI locally
set -euo pipefail

# Check for Bun
if ! command -v bun >/dev/null 2>&1; then
  echo "Error: Bun is required to build the standalone binary."
  echo "Install Bun: curl -fsSL https://bun.sh/install | bash"
  echo "Or install via npm (uses Node.js): npm install -g ."
  exit 1
fi

echo "Building standalone binary..."
bun run compile

# Determine installation directory
if [ -w "/usr/local/bin" ]; then
  DEST_DIR="/usr/local/bin"
else
  DEST_DIR="$HOME/.local/bin"
  mkdir -p "$DEST_DIR"
fi

DEST="${DEST_DIR}/gemini-api"

echo "Installing binary to ${DEST}..."
cp dist/gemini-api "$DEST"
chmod +x "$DEST"

echo "✓ Installed gemini-api to ${DEST}"

# Check if DEST_DIR is in PATH
case ":$PATH:" in
  *":$DEST_DIR:"*) ;;
  *)
    echo "Warning: $DEST_DIR is not in your PATH."
    echo "You may need to add it to your shell profile (e.g., ~/.bashrc or ~/.zshrc):"
    echo "  export PATH=\"\$PATH:$DEST_DIR\""
    ;;
esac

# Verify installation if it's in PATH
if command -v gemini-api >/dev/null 2>&1; then
  echo "Verification: $(gemini-api --version)"
else
  echo "To run it, you may need to restart your terminal or use the full path:"
  echo "  $DEST --version"
fi
