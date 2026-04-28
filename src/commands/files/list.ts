import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";
import { resolveContext, apiRequest } from "../../lib/api";
import { printCurl, printError } from "../../lib/output";
import { CLIError } from "../../lib/errors";

export default defineCommand({
  meta: {
    name: "list",
    description: `List files in environment.

Examples:
  gemini-api files list env_xyz789
  gemini-api files list env_xyz789 --dry-run`,
  },
  args: {
    ...globalFlags,
    "env-id": {
      type: "positional",
      description: "Environment ID",
      required: true,
    },
  },
  async run({ args }) {
    try {
      const ctx = resolveContext(args);
      const envId = args["env-id"];
      const url = `/environments/${envId}/files`;

      if (args["dry-run"]) {
        printCurl("GET", `${ctx.baseUrl}${url}`, ctx.apiKey);
        return;
      }

      const response = await apiRequest<any>(ctx, "GET", url);

      const files = Array.isArray(response) ? response : response.files || [];

      if (args.json) {
        console.log(JSON.stringify(response, null, 2));
      } else {
        if (files.length === 0) {
          console.log(`No files found in environment ${envId}.`);
          return;
        }

        console.log(`Files in ${envId}:\n`);
        console.log("Path".padEnd(30) + "Size".padEnd(10) + "Modified");
        console.log("─".repeat(28) + "  " + "─".repeat(8) + "  " + "─".repeat(12));

        for (const file of files) {
          const path = file.path || "";
          const size = file.size ? `${(file.size / 1024).toFixed(1)} KB` : "0 KB";
          const modified = file.modifiedTime || file.updateTime || "";
          const modifiedDate = modified ? new Date(modified).toISOString().split('T')[0] : "";

          console.log(
            path.padEnd(30) + 
            size.padEnd(10) + 
            modifiedDate
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
