// gemini-api CLI entry point
// TODO: Implement — see tasks/task_2.md

import { defineCommand, runMain } from "citty";

const main = defineCommand({
  meta: {
    name: "gemini-api",
    version: "0.2.0",
    description: "The official CLI for the Gemini API",
  },
  subCommands: {
    run: () => import("./commands/run").then((m) => m.default),
    agents: () => import("./commands/agents/index").then((m) => m.default),
    files: () => import("./commands/files/index").then((m) => m.default),
  },
});

runMain(main);
