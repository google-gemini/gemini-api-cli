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
import { apiRequest, resolveContext } from "../../lib/api";
import { CLIError } from "../../lib/errors";
import { printCurl, printError } from "../../lib/output";
import { globalFlags } from "../../lib/shared-args";

export default defineCommand({
  meta: {
    name: "list",
    description: `List deployed agents.

Examples:
  gemini-api agents list
  gemini-api agents list --json
  gemini-api agents list --dry-run`,
  },
  args: {
    ...globalFlags,
  },
  async run({ args }) {
    try {
      const ctx = resolveContext(args);
      const url = "/agents?pageSize=100";

      if (args["dry-run"]) {
        printCurl("GET", `${ctx.baseUrl}${url}`, ctx.apiKey);
        return;
      }

      const response = await apiRequest<any>(ctx, "GET", url);

      const agents = Array.isArray(response) ? response : response.agents || [];

      if (args.json) {
        console.log(JSON.stringify(response, null, 2));
      } else {
        if (agents.length === 0) {
          console.log("No agents found.");
          return;
        }

        console.log(`${"Name".padEnd(15) + "Base Agent".padEnd(15)}Created`);
        console.log(`${"─".repeat(14)}  ${"─".repeat(14)}  ${"─".repeat(12)}`);

        for (const agent of agents) {
          const name = agent.id || agent.name || "";
          const baseAgent = agent.base_agent || "";
          const created = agent.created_time || agent.createTime || "";
          const createdDate = created ? new Date(created).toISOString().split("T")[0] : "";

          console.log(name.padEnd(15) + baseAgent.padEnd(15) + createdDate);
        }
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
