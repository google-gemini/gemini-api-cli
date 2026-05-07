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
# CUJ-29: Agent full lifecycle (create → list → get → delete)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-29: Agent full lifecycle (create → list → get → delete) ==="

AGENT_NAME="e2e-lifecycle-$(date +%s)"

# 1. Init
$CLI agents init "$AGENT_NAME"

# 2. Create
$CLI agents create --path "./$AGENT_NAME"

# 3. Test Interaction
$CLI run "What is 2+2?" --agent "$AGENT_NAME"

# 4. List — should include the agent
$CLI agents list

# 5. Get — should return details
$CLI agents get "$AGENT_NAME"

# 6. Delete
$CLI agents delete "$AGENT_NAME" --force

# 7. Cleanup
rm -rf "$AGENT_NAME"
