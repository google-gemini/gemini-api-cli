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
# install.sh — Install gemini-api CLI
set -euo pipefail

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

# Map arch names
case "$ARCH" in
  x86_64) ARCH="x64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH"
    exit 1
    ;;
esac

# Map OS names
case "$OS" in
  linux) OS="linux" ;;
  darwin) OS="darwin" ;;
  *)
    echo "Unsupported OS: $OS"
    exit 1
    ;;
esac

BINARY="gemini-api-${OS}-${ARCH}"
REPO="google-gemini/gemini-api-cli"
URL="https://github.com/${REPO}/releases/latest/download/${BINARY}"

# Determine installation directory
if [ -w "/usr/local/bin" ]; then
  DEST_DIR="/usr/local/bin"
else
  DEST_DIR="$HOME/.local/bin"
  mkdir -p "$DEST_DIR"
fi

DEST="${DEST_DIR}/gemini-api"

echo "Downloading ${BINARY} from GitHub releases..."
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$URL" -o "$DEST"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$DEST" "$URL"
else
  echo "Error: curl or wget is required to download the binary."
  exit 1
fi

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
