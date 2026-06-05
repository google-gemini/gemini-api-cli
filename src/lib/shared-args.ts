// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

export const globalFlags = {
  "api-key": {
    type: "string" as const,
    description: "API key for authentication (or set GEMINI_API_KEY)",
  },
  "base-url": {
    type: "string" as const,
    description: "Override API base URL (or set GEMINI_API_BASE_URL)",
  },
  json: {
    type: "boolean" as const,
    description: "JSON output mode",
    default: false,
  },
  "dry-run": {
    type: "boolean" as const,
    description: "Print curl and exit",
    default: false,
  },
  verbose: {
    type: "boolean" as const,
    alias: "v",
    description: "Verbose output (JSON lines per step, full details)",
    default: false,
  },
};
