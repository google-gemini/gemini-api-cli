// TODO: Implement — see tasks/task_11.md

import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";

export default defineCommand({
  meta: {
    name: "download",
    description: `Download files from environment.

Examples:
  gemini-api files download env_xyz789
  gemini-api files download env_xyz789 --output ./results`,
  },
  args: {
    ...globalFlags,
    "env-id": {
      type: "positional",
      description: "Environment ID",
      required: true,
    },
    output: {
      type: "string",
      description: "Output directory",
    },
  },
  run({ args }) {
    console.log("TODO: gemini-api files download", args["env-id"]);
  },
});
