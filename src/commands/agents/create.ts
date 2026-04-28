// TODO: Implement — see tasks/task_8.md

import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";

export default defineCommand({
  meta: {
    name: "create",
    description: `Deploy the agent from the current directory to the platform.

Examples:
  gemini-api agents create
  gemini-api agents create --path ./my-agent
  gemini-api agents create --dry-run`,
  },
  args: {
    ...globalFlags,
    path: {
      type: "string",
      description: "Path to agent directory",
      default: ".",
    },
    "base-env": {
      type: "string",
      description: "Override base environment",
    },
  },
  run({ args }) {
    console.log("TODO: gemini-api agents create", args);
  },
});
