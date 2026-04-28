// TODO: Implement — see tasks/task_8.md

import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";

export default defineCommand({
  meta: {
    name: "init",
    description: `Scaffold new agent project.

Examples:
  gemini-api agents init my-agent
  gemini-api agents init my-agent --dry-run`,
  },
  args: {
    ...globalFlags,
    name: {
      type: "positional",
      description: "Name of the agent project",
      required: true,
    },
  },
  run({ args }) {
    console.log("TODO: gemini-api agents init", args.name);
  },
});
