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
import { apiRequest, resolveContext, type Source } from "../../lib/api";
import { loadAgent } from "../../lib/config";
import { CLIError, ConfigError } from "../../lib/errors";
import { collectInlineFiles, getEnvKeys } from "../../lib/files";
import { printCurl, printError } from "../../lib/output";
import { globalFlags } from "../../lib/shared-args";

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

      const body: Record<string, unknown> = {
        name: config.id,
        base_agent: config.base_agent,
      };

      if (config.instructions) {
        body.system_instruction = config.instructions;
      }

      // Handle base_environment
      if (config.base_environment) {
        body.base_environment = config.base_environment;
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

        if (sources.length > 0) {
          body.base_environment = {
            type: "remote",
            sources: sources,
          };
        }

        // Handle .env credential injection
        if (inlineFiles.length > 0) {
          const envFile = inlineFiles.find((f) => f.target === "/credentials/.env");
          if (envFile) {
            const keys = getEnvKeys(envFile.content);
            if (keys.length > 0) {
              const note = `\n\nNote: Credentials are stored in /credentials/.env. They are not available as environment variables yet. Please add them as environment variables if you want to use them in your Skills. Available variables: ${keys.join(", ")}.`;
              body.system_instruction = (body.system_instruction || "") + note;
            }
          }
        }
      }

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
