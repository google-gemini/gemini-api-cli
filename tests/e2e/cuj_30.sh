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
# CUJ-30: Agent test (interaction via local config)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-30: Agent test (interaction via local config) ==="

AGENT_NAME="e2e-test-$(date +%s)"
$CLI agents init "$AGENT_NAME"

# dry-run
$CLI agents test --prompt "Hello" --path "./$AGENT_NAME" --dry-run

# live
$CLI agents test --prompt "Hello" --path "./$AGENT_NAME"

rm -rf "$AGENT_NAME"
