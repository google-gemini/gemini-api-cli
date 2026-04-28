#!/bin/bash
set -euo pipefail

TARGETS=(
  "bun-linux-x64:gemini-api-linux-x64"
  "bun-linux-arm64:gemini-api-linux-arm64"
  "bun-darwin-x64:gemini-api-darwin-x64"
  "bun-darwin-arm64:gemini-api-darwin-arm64"
  "bun-windows-x64:gemini-api-win-x64.exe"
)

mkdir -p dist

for entry in "${TARGETS[@]}"; do
  target="${entry%%:*}"
  output="${entry##*:}"
  echo "Building ${output}..."
  bun build --compile --target="${target}" ./src/cli.ts --outfile "dist/${output}"
done

echo "✓ All binaries built in dist/"
ls -lh dist/
