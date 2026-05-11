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
import * as yaml from "js-yaml";
import { apiRequest, resolveContext } from "../../lib/api";
import { CLIError } from "../../lib/errors";
import { printCurl, printError } from "../../lib/output";
import { globalFlags } from "../../lib/shared-args";

export default defineCommand({
  meta: {
    name: "get",
    description: `Get agent details.

Examples:
  gemini-api agents get my-agent
  gemini-api agents get my-agent --json
  gemini-api agents get my-agent --dry-run`,
  },
  args: {
    ...globalFlags,
    id: {
      type: "positional",
      description: "Agent ID",
      required: true,
    },
  },
  async run({ args }) {
    try {
      const ctx = resolveContext(args);
      const url = `/agents/${args.id}`;

      if (args["dry-run"]) {
        printCurl("GET", `${ctx.baseUrl}${url}`, ctx.apiKey);
        return;
      }

      const response = await apiRequest<any>(ctx, "GET", url);

      if (args.json) {
        console.log(JSON.stringify(response, null, 2));
      } else {
        console.log(yaml.dump(response));
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
