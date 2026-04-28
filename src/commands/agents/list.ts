// TODO: Implement — see tasks/task_8.md

import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";

export default defineCommand({
  meta: {
    name: "list",
    description: `List deployed agents.

Examples:
  gemini-api agents list
  gemini-api agents list --dry-run`,
  },
  args: {
    ...globalFlags,
  },
  run({ args }) {
    console.log("TODO: gemini-api agents list", args);
  },
});
