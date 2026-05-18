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

# CUJ-45: Agent test with network transform (dry-run)
source ~/.bash_profile
CLI="bun run src/cli.ts"

AGENT_NAME="cuj-45-agent-$(date +%s)"

echo "=== Running CUJ-45: Agent test with network transform (dry-run) ==="

# 1. Initialize agent project
$CLI agents init "$AGENT_NAME" > /dev/null

# 2. Inject custom agent.yaml with network transforms
cat <<EOF > "$AGENT_NAME/agent.yaml"
id: $AGENT_NAME
base_agent: antigravity-preview-05-2026
tools:
  - type: code_execution
environment:
  type: "remote"
  sources:
    - type: "gcs"
      source: "gs://test-bucket/data"
      target: ".agents/workspace"
  network:
    allowlist:
      - domain: "api.github.com"
        transform:
          Authorization: "Bearer ghp_secret_oauth_token"
      - domain: "*"
EOF

# 3. Dry-run and capture curl output
OUTPUT=$($CLI agents test --prompt "Hello" --path "./$AGENT_NAME" --dry-run 2>&1)

# 4. Assert that network transforms are correctly preserved and printed in the dry-run payload
if echo "$OUTPUT" | grep -q "ghp_secret_oauth_token"; then
  echo "✓ SUCCESS: Network transform header was found in dry-run request payload!"
else
  echo "✗ FAILED: Network transform header was missing from dry-run request payload!"
  echo "Output:"
  echo "$OUTPUT"
  rm -rf "$AGENT_NAME"
  exit 1
fi

# 5. Cleanup
rm -rf "$AGENT_NAME"
echo "✓ Cleanup complete."
