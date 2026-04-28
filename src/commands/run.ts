// gemini-api run command
// TODO: Implement — see tasks/task_5.md

import { defineCommand } from "citty";
import { globalFlags } from "../lib/shared-args";

export default defineCommand({
  meta: {
    name: "run",
    description: `Create an interaction against a model or agent.

Examples:
  gemini-api run "What is the capital of France?"
  gemini-api run "Explain quantum computing" --model gemini-3-pro-preview`,
  },
  args: {
    ...globalFlags,
    prompt: {
      type: "positional",
      description: "Input prompt. Use '-' for stdin.",
      required: false,
    },
    model: {
      type: "string",
      alias: "m",
      description: "Model to use",
      default: "gemini-3-flash-preview",
    },
    agent: {
      type: "string",
      alias: "a",
      description: "Agent to use (overrides --model)",
    },
    stream: {
      type: "boolean",
      description: "Stream response via SSE",
      default: true,
    },
  },
  run({ args }) {
    if (!args.prompt) {
      console.error("✗ Missing required argument: prompt\n");
      console.error("  Try:");
      console.error("    gemini-api run \"What is the capital of France?\"");
      console.error("    gemini-api run \"Explain quantum computing\" --model gemini-3-pro-preview");
      process.exit(1);
    }
    console.log("TODO: gemini-api run", args.prompt);
  },
});
