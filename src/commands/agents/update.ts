// TODO: Implement — see tasks/task_8.md

import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";

export default defineCommand({
  meta: {
    name: "update",
    description: `Update deployed agent.

Examples:
  gemini-api agents update my-agent
  gemini-api agents update my-agent --path ./my-agent`,
  },
  args: {
    ...globalFlags,
    id: {
      type: "positional",
      description: "Agent ID",
      required: true,
    },
    path: {
      type: "string",
      description: "Path to agent directory",
      default: ".",
    },
  },
  run({ args }) {
    console.log("TODO: gemini-api agents update", args.id);
  },
});
