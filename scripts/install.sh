#!/bin/bash
# install.sh — Install gemini-api CLI from GCS
set -euo pipefail

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

# Map arch names
case "$ARCH" in
  x86_64) ARCH="x64" ;;
  aarch64|arm64) ARCH="arm64" ;;
esac

BINARY="gemini-api-${OS}-${ARCH}"
DEST="${INSTALL_DIR:-/usr/local/bin}/gemini-api"

echo "Downloading ${BINARY}..."
gcloud storage cp "gs://gemini-api-eap/agents-api/${BINARY}" "${DEST}"
chmod +x "${DEST}"

echo "✓ Installed gemini-api to ${DEST}"
gemini-api --version
