// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import { defineCommand } from "citty";
import {
  apiStreamRequest,
  buildInteractionRequest,
  isAgentName,
  type RunOptions,
  resolveContext,
  type Source,
} from "../../lib/api";
import { loadAgent } from "../../lib/config";
import { CLIError, ConfigError } from "../../lib/errors";
import { collectInlineFiles, getEnvKeys } from "../../lib/files";
import { logRequest, logResponse } from "../../lib/logger";
import {
  HumanStreamRenderer,
  printCompletionSummary,
  printCurl,
  printError,
} from "../../lib/output";
import { globalFlags } from "../../lib/shared-args";
import { processStream } from "../../lib/stream";

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

      // system_instruction from agent.yaml is sent in the request body.
      // AGENTS.md is NOT loaded here — it is uploaded as an inline file and
      // merged with the system instruction on the server side.
      let systemInstruction = config.instructions;

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

      // Build environment config
      let environment: Record<string, unknown> = {};
      if (args.environment) {
        environment = { env_id: args.environment };
      } else {
        const sources: Source[] = [...inlineFiles] as unknown as Source[];
        if (config.sources) {
          sources.push(...config.sources);
        }

        if (sources.length > 0) {
          environment = { type: "remote", sources: sources };
        } else if (config.environment) {
          // Use environment from agent.yaml (e.g. { enabled: true })
          environment = config.environment;
        }
      }

      // Determine whether base_agent is an agent or model name.
      // Known agents use the `agent` field; model names use the `model` field.
      const isAgent = isAgentName(config.base_agent);

      const runOpts: RunOptions = {
        agent: isAgent ? config.base_agent : undefined,
        model: isAgent ? undefined : config.base_agent,
        input: prompt,
        systemInstruction: systemInstruction,
        tools: config.tools as any,
        previousInteractionId: args["previous-interaction-id"] as string | undefined,
        stream: true,
        environment: Object.keys(environment).length > 0 ? environment : undefined,
      };

      const body = buildInteractionRequest(runOpts);

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
          onEvent: (event, block) => {
            if (event.type === "step.start" || event.type === "step.stop") {
              renderer.handleStepEvent(event);
            } else if (event.type === "content.start") {
              activeBlocks.set(event.data.index, event.data.content.type);
            } else if (event.type === "content.delta") {
              const type = activeBlocks.get(event.data.index) || "text";
              renderer.handleEvent(event, type, block);
            }
          },
          onComplete: (result) => {
            renderer.finish();
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
