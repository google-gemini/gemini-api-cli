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

// gemini-api CLI entry point
// TODO: Implement — see tasks/task_2.md

import { defineCommand, runMain } from "citty";

const main = defineCommand({
  meta: {
    name: "gemini-api",
    version: "0.1.0",
    description: "CLI to access Gemini API",
  },
  subCommands: {
    run: () => import("./commands/run").then((m) => m.default),
    agents: () => import("./commands/agents/index").then((m) => m.default),
    files: () => import("./commands/files/index").then((m) => m.default),
  },
});

if (process.argv.includes("--help") || process.argv.includes("-h")) {
  process.env.CONSOLA_LEVEL = "5";
}

runMain(main);
