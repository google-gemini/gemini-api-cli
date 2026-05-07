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
# CUJ-36: Download environment files (snapshot)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-36: Download environment files ==="

# dry-run
$CLI files download env_fake123 --dry-run

# live
echo "Creating test file in environment..."
RESULT=$($CLI run "Write 'hello world' to test.txt" --agent waverunner --json 2>&1)
ENV_ID=$(echo "$RESULT" | grep -o '"environment_id":"[^"]*"' | head -1 | cut -d'"' -f4)

if [ -z "$ENV_ID" ]; then
  # Try fallback to search in full output if json parsing failed or format differs
  ENV_ID=$(echo "$RESULT" | grep -o 'environment_id: [a-zA-Z0-9-]*' | head -1 | cut -d' ' -f2)
fi

if [ -z "$ENV_ID" ]; then
  echo "Failed to get environment ID."
  echo "Result was:"
  echo "$RESULT"
  exit 1
fi

echo "Found Environment ID: $ENV_ID"

echo "Downloading snapshot for environment $ENV_ID..."
$CLI files download "$ENV_ID" --output ./tmp

echo "Verifying extracted files..."
if [ -f "./tmp/snapshot_$ENV_ID/test.txt" ]; then
  echo "✓ Verification successful: test.txt found."
  rm -rf "./tmp/snapshot_$ENV_ID"
else
  echo "✗ Verification failed: test.txt not found."
  exit 1
fi
