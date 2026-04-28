// TODO: Implement — see tasks/task_8.md

import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";

export default defineCommand({
  meta: {
    name: "get",
    description: `Get agent details.

Examples:
  gemini-api agents get my-agent
  gemini-api agents get my-agent --json`,
  },
  args: {
    ...globalFlags,
    id: {
      type: "positional",
      description: "Agent ID",
      required: true,
    },
  },
  run({ args }) {
    console.log("TODO: gemini-api agents get", args.id);
  },
});
