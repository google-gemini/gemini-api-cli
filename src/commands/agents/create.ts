import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";
import { loadAgent } from "../../lib/config";
import { resolveContext, apiRequest } from "../../lib/api";
import { printCurl, printError } from "../../lib/output";
import { CLIError, ConfigError } from "../../lib/errors";

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
  async run({ args }) {
    try {
      const ctx = resolveContext(args);
      const agentDir = args.path;
      const baseEnvOverride = args["base-env"];

      const { config } = await loadAgent(agentDir);

      const body: any = {
        ...config,
      };

      if (baseEnvOverride) {
        body.base_environment = baseEnvOverride;
      }

      const url = "/agents";

      if (args["dry-run"]) {
        printCurl("POST", `${ctx.baseUrl}${url}`, ctx.apiKey, body);
        return;
      }

      const response = await apiRequest<any>(ctx, "POST", url, body);

      if (args.json) {
        console.log(JSON.stringify(response, null, 2));
      } else {
        console.log(`✓ Created agent: agents/${response.name || response.id}`);
        console.log(`  agent_id: ${response.id}`);
        console.log(`  base_agent: ${response.base_agent}`);
        if (response.environment) {
          console.log(`  environment: ${response.environment}`);
        }
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
