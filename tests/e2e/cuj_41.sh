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
# CUJ-41: Dry-run on all commands
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-41: Dry-run on all commands ==="

$CLI run "Hello" --dry-run
$CLI agents create --dry-run
$CLI agents list --dry-run
$CLI agents get my-agent --dry-run
$CLI agents delete my-agent --force --dry-run
$CLI files list env_123 --dry-run
$CLI files download env_123 --dry-run
