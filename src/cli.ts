// gemini-api CLI entry point
// TODO: Implement — see tasks/task_2.md

import { defineCommand, runMain } from "citty";

const main = defineCommand({
  meta: {
    name: "gemini-api",
    version: "0.2.0",
        description: "CLI to access Gemini API",
  },
  subCommands: {
    run: () => import("./commands/run").then((m) => m.default),
    agents: () => import("./commands/agents/index").then((m) => m.default),
    files: () => import("./commands/files/index").then((m) => m.default),
  },
});

if (process.argv.includes('--help') || process.argv.includes('-h')) {
  process.env.CONSOLA_LEVEL = '5';
}

runMain(main);

