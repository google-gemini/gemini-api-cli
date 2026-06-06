import { defineCommand } from "citty";
import {
  apiRequest,
  normalizeSources,
  resolveContext,
  type Source,
  validateSources,
} from "../../lib/api";
import { loadAgent } from "../../lib/config";
import { CLIError, ConfigError } from "../../lib/errors";
import { collectInlineFiles } from "../../lib/files";
import { printCurl, printError } from "../../lib/output";
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
    env: {
      type: "string",
      alias: "e",
      description: "Load environment variables from a .env file",
    },
  },
  async run({ args }) {
    try {
      const agentDir = args.path as string;
      const baseEnvOverride = args["base-env"] as string | undefined;
      const envFile = args.env as string | undefined;
      const ctx = resolveContext(args);

      const { config } = await loadAgent(agentDir, { envFile });

      const body: Record<string, unknown> = {
        id: config.id,
        base_agent: config.base_agent,
      };

      // Only send base_agent and instructions for now to debug
      /*
      if (config.description) body.description = config.description;
      if (config.tools) body.tools = config.tools;
      if (config.subagents) body.subagents = config.subagents;
      if (config.metadata) body.metadata = config.metadata;
      */

      if (config.instructions) {
        body.system_instruction = config.instructions;
      }

      // Handle base_environment
      if (baseEnvOverride) {
        body.base_environment = baseEnvOverride;
      } else if (config.base_environment) {
        if (typeof config.base_environment === "string") {
          body.base_environment = config.base_environment;
        } else if (typeof config.base_environment === "object") {
          const baseEnvObj = config.base_environment as any;
          if (baseEnvObj.sources) {
            const normalizedBaseSources = normalizeSources(baseEnvObj.sources);
            validateSources(normalizedBaseSources);
            baseEnvObj.sources = normalizedBaseSources;
          }
          body.base_environment = baseEnvObj;
        }
      } else {
        // Collect and inline files from the agent directory
        const inlineFiles = await collectInlineFiles(agentDir);

        const sources: Source[] = [...inlineFiles] as unknown as Source[];
        if (config.sources) {
          sources.push(...config.sources);
        }

        // Also merge sources from environment.sources (e.g. gcs, github)
        const env = config.environment as Record<string, unknown> | undefined;
        if (env && env.type === "remote" && Array.isArray(env.sources)) {
          sources.push(...(env.sources as Source[]));
        }

        const normalized = normalizeSources(sources);
        validateSources(normalized);

        if (normalized && normalized.length > 0) {
          body.base_environment = {
            type: "remote",
            sources: normalized,
          };
          const envObj = config.environment as any;
          if (envObj && typeof envObj === "object" && envObj.network) {
            (body.base_environment as any).network = envObj.network;
          }
        }
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
        console.log(`✓ Created agent: ${response.name || response.id}`);
        const agentId = response.id || (response.name ? response.name.split("/").pop() : "unknown");
        console.log(`  agent_id: ${agentId}`);
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
