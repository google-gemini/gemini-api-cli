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
