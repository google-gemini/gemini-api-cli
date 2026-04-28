import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";
import { loadAgent } from "../../lib/config";
import { resolveContext, buildInteractionRequest, apiStreamRequest, apiRequest, type RunOptions, parseToolFlag, type Tool } from "../../lib/api";
import { processStream } from "../../lib/stream";
import { printCurl, HumanStreamRenderer, printCompletionSummary, printError } from "../../lib/output";
import { CLIError, ConfigError } from "../../lib/errors";
import { logRequest, logResponse } from "../../lib/logger";
import { collectInlineFiles, getEnvKeys } from "../../lib/files";
import * as fs from "node:fs";
import * as path from "node:path";

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
    "previous-interaction-id": {
      type: "string",
      description: "Continue from previous interaction",
    },
    environment: {
      type: "string",
      description: "Use existing environment",
    },
  },
  async run({ args }) {
    try {
      const agentDir = args.path as string;
      const prompt = args.prompt as string;
      
      const sharedFlags = {
        apiKey: (args["api-key"] || args.apiKey) as string | undefined,
        baseUrl: (args["base-url"] || args.baseUrl) as string | undefined,
        json: args.json as boolean,
        dryRun: (args["dry-run"] || args.dryRun) as boolean,
      };

      const ctx = resolveContext(sharedFlags);

      const { config } = await loadAgent(agentDir);
      const inlineFiles = await collectInlineFiles(agentDir);

      let systemInstruction = config.system_instruction;

      // Add note about inline files if any exist
      if (inlineFiles.length > 0) {
        const note = `\n\nNote: Files from your agent directory have been seeded into the environment under /.agents/.`;
        if (systemInstruction) {
          systemInstruction += note;
        } else {
          systemInstruction = note;
        }
      }

      // Update system instructions if .env was inlined
      const envFile = inlineFiles.find((f) => f.target === "/credentials/.env");
      if (envFile) {
        const keys = getEnvKeys(envFile.content);
        if (keys.length > 0) {
          const note = `\n\nNote: Credentials are stored in /credentials/.env. They are not available as environment variables yet. Please add them as environment variables if you want to use them in your Skills. Available variables: ${keys.join(", ")}.`;
          if (systemInstruction) {
            systemInstruction += note;
          } else {
            systemInstruction = note;
          }
        }
      }

      const runOpts: RunOptions = {
        agent: config.base_agent,
        input: prompt,
        systemInstruction: systemInstruction,
        tools: config.tools as any, // Cast to any to avoid lint error for now
        previousInteractionId: args["previous-interaction-id"] as string | undefined,
        stream: true,
      };

      const body = buildInteractionRequest(runOpts);

      let environment: any = {};
      if (args.environment) {
        environment = { env_id: args.environment };
      } else {
        if (config.environment) {
          environment = { ...config.environment };
        }
        if (inlineFiles.length > 0) {
          environment.config = environment.config || {};
          environment.config.sources = inlineFiles;
        }
      }

      if (Object.keys(environment).length > 0) {
        (body as any).environment = environment;
      }

      if (args["dry-run"]) {
        printCurl("POST", `${ctx.baseUrl}/interactions`, ctx.apiKey, body);
        return;
      }

      const startTime = performance.now();
      
      const response = await apiStreamRequest(ctx, "/interactions", body);
      
      if (args.json) {
        await processStream(response, {
          onEvent: (event) => {
            console.log(event.raw);
          },
          onComplete: () => {},
        });
      } else {
        const renderer = new HumanStreamRenderer();
        const activeBlocks = new Map<number, string>();
        
        await processStream(response, {
          onEvent: (event) => {
            if (event.type === "content.start") {
              activeBlocks.set(event.data.index, event.data.content.type);
            } else if (event.type === "content.delta") {
              const type = activeBlocks.get(event.data.index) || "text";
              renderer.handleEvent(event, type);
            }
          },
          onComplete: (result) => {
            const latencySeconds = (performance.now() - startTime) / 1000;
            printCompletionSummary(result, latencySeconds);
            if (!args.json) {
              logRequest(result.interactionId, body);
              logResponse(result.interactionId, result);
            }
          },
        });
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
