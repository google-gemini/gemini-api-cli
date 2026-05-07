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
# CUJ-14: Image editing (input image + output image)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-14: Image editing (input image + output image) ==="

# Create test image
echo "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==" | base64 -d > tmp/test_edit.png

# dry-run
$CLI run "Make it green" --input image:tmp/test_edit.png --response-modality image --model gemini-3-pro-image-preview --output tmp/test_edited.png --dry-run

# live
$CLI run "Make this image green" --input image:tmp/test_edit.png --response-modality image --model gemini-3-pro-image-preview --output tmp/test_edited.png
