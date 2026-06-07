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
  DEFAULT_SERVER_TIMEOUT_SECONDS,
  isAgentName,
  normalizeSources,
  type RunOptions,
  resolveContext,
  SharedFlags,
  type Source,
  validateSources,
} from "../../lib/api";
import { loadAgent } from "../../lib/config";
import { CLIError, ConfigError } from "../../lib/errors";
import { TestArgsSchema } from "../../lib/schemas";
import { collectInlineFiles } from "../../lib/files";
import { logRequest, logResponse } from "../../lib/logger";
import {
  HumanStreamRenderer,
  mapContentToStepEvent,
  printCompletionSummary,
  printCurl,
  printError,
  renderStepEvent,
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
    timeout: {
      type: "string",
      description: "Override timeout in seconds",
    env: {
      type: "string",
      alias: "e",
      description: "Load environment variables from a .env file",
    },
  },
  async run({ args }) {
    try {
      const parseResult = TestArgsSchema.safeParse(args);
      if (!parseResult.success) {
        const errors = parseResult.error.issues
          .map((i) => `  - ${i.path.join(".")}: ${i.message}`)
          .join("\n");
        throw new CLIError(`Invalid arguments:\n${errors}`);
      }
      const parsedArgs = parseResult.data;

      const agentDir = parsedArgs.path;
      const prompt = parsedArgs.prompt;

      const agentDir = args.path as string;
      const prompt = args.prompt as string;
      const envFile = args.env as string | undefined;
      const sharedFlags = {
        apiKey: parsedArgs["api-key"],
        baseUrl: parsedArgs["base-url"],
        json: parsedArgs.json,
        dryRun: parsedArgs["dry-run"],
      };

      const ctx = resolveContext(sharedFlags as SharedFlags);

      const { config } = await loadAgent(agentDir, { envFile });
      const inlineFiles = await collectInlineFiles(agentDir);

      // system_instruction from agent.yaml is sent in the request body.
      // AGENTS.md is NOT loaded here — it is uploaded as an inline file and
      // merged with the system instruction on the server side.
      const systemInstruction = config.instructions;

      // Build environment config
      let environment: any;
      if (parsedArgs.environment) {
        environment = parsedArgs.environment;
      } else {
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
          environment = { type: "remote", sources: normalized };
          const envObj = config.environment as any;
          if (envObj && typeof envObj === "object" && envObj.network) {
            environment.network = envObj.network;
          }
        } else if (config.environment) {
          if (typeof config.environment === "string") {
            environment = config.environment;
          } else if (typeof config.environment === "object") {
            const envObj = config.environment as any;
            if (envObj.sources) {
              const normalizedEnvSources = normalizeSources(envObj.sources);
              validateSources(normalizedEnvSources);
              envObj.sources = normalizedEnvSources;
            }
            environment = envObj;
          }
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
        previousInteractionId: parsedArgs["previous-interaction-id"],
        stream: true,
        environment: environment || undefined,
      };

      const body = buildInteractionRequest(runOpts);

      const timeoutSeconds = parsedArgs.timeout ?? config.timeout ?? DEFAULT_SERVER_TIMEOUT_SECONDS;
      const headers = {
        "x-server-timeout": timeoutSeconds.toString(),
      };

      if (parsedArgs["dry-run"]) {
        printCurl("POST", `${ctx.baseUrl}/interactions`, ctx.apiKey, body, headers);
        return;
      }

      const startTime = performance.now();

      const response = await apiStreamRequest(ctx, "/interactions", body, headers);

      if (parsedArgs.json) {
        await processStream(response, {
          onEvent: (event) => {
            console.log(event.raw);
          },
          onComplete: () => {},
        });
      } else {
        const verbose = parsedArgs.verbose;
        const renderer = new HumanStreamRenderer(process.stdout, verbose);

        await processStream(response, {
          onEvent: (event, block) => {
            const mapped = mapContentToStepEvent(event);
            renderStepEvent(renderer, mapped, block);
          },
          onComplete: (result) => {
            renderer.finish();
            const latencySeconds = (performance.now() - startTime) / 1000;
            printCompletionSummary(result, latencySeconds, verbose);
            if (!parsedArgs.json) {
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
