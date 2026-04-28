import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";
import { resolveContext, apiRequest } from "../../lib/api";
import { printCurl, printError } from "../../lib/output";
import { CLIError } from "../../lib/errors";
import * as yaml from "js-yaml";

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
