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
# CUJ-34: Deep Research (background mode)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-34: Deep Research (background mode) ==="

# dry-run
$CLI run "Research the history of TPUs" --agent deep-research-preview-04-2026 --dry-run

# live (will take minutes)
$CLI run "Research the history of Google TPUs in 2 paragraphs" --agent deep-research-preview-04-2026
