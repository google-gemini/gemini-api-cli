// TODO: Implement — see tasks/task_8.md

import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";

export default defineCommand({
  meta: {
    name: "delete",
    description: `Delete deployed agent.

Examples:
  gemini-api agents delete my-agent
  gemini-api agents delete my-agent --force`,
  },
  args: {
    ...globalFlags,
    id: {
      type: "positional",
      description: "Agent ID",
      required: true,
    },
    force: {
      type: "boolean",
      description: "Skip confirmation",
      default: false,
    },
  },
  run({ args }) {
    console.log("TODO: gemini-api agents delete", args.id);
  },
});
