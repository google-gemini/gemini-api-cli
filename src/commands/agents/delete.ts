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

import * as readline from "node:readline";
import { defineCommand } from "citty";
import { apiRequest, resolveContext } from "../../lib/api";
import { CLIError } from "../../lib/errors";
import { printCurl, printError } from "../../lib/output";
import { globalFlags } from "../../lib/shared-args";

export default defineCommand({
  meta: {
    name: "delete",
    description: `Delete deployed agent.

Examples:
  gemini-api agents delete my-agent
  gemini-api agents delete my-agent --force
  gemini-api agents delete my-agent --dry-run`,
  },
  args: {
    ...globalFlags,
    id: {
      type: "positional",
      description: "Agent ID",
      required: true,
    },
    force: {
      type: "boolean",
      description: "Skip confirmation",
      default: false,
    },
  },
  async run({ args }) {
    try {
      const ctx = resolveContext(args);
      const id = args.id;
      const force = args.force;

      if (!id || id.trim() === "") {
        printError("Error: Agent ID cannot be empty.");
        process.exit(1);
      }

      const url = `/agents/${id}`;

      if (args["dry-run"]) {
        printCurl("DELETE", `${ctx.baseUrl}${url}`, ctx.apiKey);
        return;
      }

      const performDelete = async () => {
        const response = await apiRequest<any>(ctx, "DELETE", url);
        if (args.json) {
          console.log(JSON.stringify(response, null, 2));
        } else {
          console.log(`✓ Deleted agent: agents/${id}`);
        }
      };

      if (force || !process.stdout.isTTY) {
        await performDelete();
        return;
      }

      const confirmed = await new Promise<boolean>((resolve) => {
        const rl = readline.createInterface({
          input: process.stdin,
          output: process.stdout,
        });

        rl.question(
          `⚠ This will permanently delete agent '${id}'.\n  Are you sure? [y/N] `,
          (answer) => {
            rl.close();
            resolve(answer.toLowerCase() === "y");
          },
        );
      });

      if (confirmed) {
        await performDelete();
      } else {
        console.log("Aborted.");
      }
    } catch (error) {
      if (error instanceof CLIError) {
        printError(error.message);
      } else {
        printError(`Unexpected error: ${(error as Error).message}`);
      }
      process.exit(1);
    }
  },
});
