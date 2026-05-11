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
import { globalFlags } from "../../lib/shared-args";
import { loadAgent } from "../../lib/config";
import { resolveContext, apiRequest } from "../../lib/api";
import { printCurl, printError } from "../../lib/output";
import { CLIError, ConfigError } from "../../lib/errors";
import { collectInlineFiles, getEnvKeys } from "../../lib/files";
import * as fs from "node:fs";
import * as path from "node:path";

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
      const agentDir = args.path as string;
      const baseEnvOverride = args["base-env"] as string | undefined;

      const { config } = await loadAgent(agentDir);

      const body: any = {
        name: config.id,
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
        // base_environment mode: skip file inlining, use the env_id directly
        body.base_environment = config.base_environment;
      } else {
        // Collect and inline files from the agent directory
        const inlineFiles = await collectInlineFiles(agentDir);
        
        const sources: any[] = [...inlineFiles];
        if (config.sources) {
          sources.push(...config.sources);
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
        const agentId = response.id || (response.name ? response.name.split('/').pop() : 'unknown');
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
