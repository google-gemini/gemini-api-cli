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

// gemini-api agents subcommand group
// TODO: Implement — see tasks/task_8.md

import { defineCommand } from "citty";

export default defineCommand({
  meta: {
    name: "agents",
    description: "Manage the agent lifecycle: init, create, list, get, update, delete, test",
  },
  subCommands: {
    init: () => import("./init").then((m) => m.default),
    create: () => import("./create").then((m) => m.default),
    list: () => import("./list").then((m) => m.default),
    get: () => import("./get").then((m) => m.default),
    update: () => import("./update").then((m) => m.default),
    delete: () => import("./delete").then((m) => m.default),
    test: () => import("./test").then((m) => m.default),
  },
});
