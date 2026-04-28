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
