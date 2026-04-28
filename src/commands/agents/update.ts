import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";
import { loadAgent } from "../../lib/config";
import { resolveContext, apiRequest } from "../../lib/api";
import { printCurl, printError } from "../../lib/output";
import { CLIError, ConfigError } from "../../lib/errors";

export default defineCommand({
  meta: {
    name: "update",
    description: `Update deployed agent.

Examples:
  gemini-api agents update my-agent
  gemini-api agents update my-agent --path ./my-agent
  gemini-api agents update my-agent --dry-run`,
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
  async run({ args }) {
    try {
      const ctx = resolveContext(args);
      const agentDir = args.path;
      const id = args.id;

      const { config } = await loadAgent(agentDir);

      const body: any = {
        ...config,
      };

      const url = `/agents/${id}`;

      if (args["dry-run"]) {
        printCurl("PATCH", `${ctx.baseUrl}${url}`, ctx.apiKey, body);
        return;
      }

      const response = await apiRequest<any>(ctx, "PATCH", url, body);

      if (args.json) {
        console.log(JSON.stringify(response, null, 2));
      } else {
        console.log(`✓ Updated agent: agents/${id}`);
      }
    } catch (error) {
      if (error instanceof CLIError || error instanceof ConfigError) {
        printError(error.message);
      } else {
        printError(`Unexpected error: ${(error as Error).message}`);
      }
      process.exit(1);
    }
  },
});
