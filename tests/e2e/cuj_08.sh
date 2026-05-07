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
# CUJ-08: Stateful multi-turn with previous-interaction-id
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-08: Stateful multi-turn with previous-interaction-id ==="

# dry-run
$CLI run "What was the word?" --previous-interaction-id fake_id_123 --dry-run

# live (two-step)
# Turn 1:
RESULT=$($CLI run "Remember the word: banana" --json 2>&1)
INT_ID=$(echo "$RESULT" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

# Turn 2:
$CLI run "What word did I ask you to remember?" --previous-interaction-id "$INT_ID"
