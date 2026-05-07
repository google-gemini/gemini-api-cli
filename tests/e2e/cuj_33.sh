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
# CUJ-33: Agent with environment persistence (multi-turn)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-33: Agent with environment persistence (multi-turn) ==="

# dry-run
$CLI run "Continue" --agent waverunner --previous-interaction-id fake_int --dry-run

# live (two-step)
RESULT=$($CLI run "Write 'hello' to tmp/test.txt" --agent waverunner --json 2>&1)
INT_ID=$(echo "$RESULT" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

$CLI run "Read tmp/test.txt and tell me what it says" --agent waverunner --previous-interaction-id "$INT_ID"
