// TODO: Implement — see tasks/task_11.md

import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";

export default defineCommand({
  meta: {
    name: "list",
    description: `List files in environment.

Examples:
  gemini-api files list env_xyz789
  gemini-api files list env_xyz789 --dry-run`,
  },
  args: {
    ...globalFlags,
    "env-id": {
      type: "positional",
      description: "Environment ID",
      required: true,
    },
  },
  run({ args }) {
    console.log("TODO: gemini-api files list", args["env-id"]);
  },
});
