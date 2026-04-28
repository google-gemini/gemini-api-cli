// TODO: Implement — see tasks/task_8.md

import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";

export default defineCommand({
  meta: {
    name: "test",
    description: `Run interaction against local agent config.

Examples:
  gemini-api agents test --prompt "Hello"
  gemini-api agents test --prompt "Hello" --path ./my-agent`,
  },
  args: {
    ...globalFlags,
    prompt: {
      type: "string",
      description: "Input prompt",
      required: true,
    },
    path: {
      type: "string",
      description: "Path to agent directory",
      default: ".",
    },
  },
  run({ args }) {
    console.log("TODO: gemini-api agents test", args.prompt);
  },
});
