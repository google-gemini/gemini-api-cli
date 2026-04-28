import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";
import { resolveContext, apiRequest } from "../../lib/api";
import { printCurl, printError } from "../../lib/output";
import { CLIError } from "../../lib/errors";

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
      const url = "/agents";

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

        console.log("Name".padEnd(15) + "Base Agent".padEnd(15) + "Created");
        console.log("─".repeat(14) + "  " + "─".repeat(14) + "  " + "─".repeat(12));

        for (const agent of agents) {
          const name = agent.id || agent.name || "";
          const baseAgent = agent.base_agent || "";
          const created = agent.created_time || agent.createTime || "";
          const createdDate = created ? new Date(created).toISOString().split('T')[0] : "";

          console.log(
            name.padEnd(15) + 
            baseAgent.padEnd(15) + 
            createdDate
          );
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
