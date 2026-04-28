import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";
import { resolveContext, apiRequest } from "../../lib/api";
import { printCurl, printError } from "../../lib/output";
import { CLIError } from "../../lib/errors";
import * as fs from "node:fs";
import * as path from "node:path";

export default defineCommand({
  meta: {
    name: "download",
    description: `Download files from environment.

Examples:
  gemini-api files download env_xyz789
  gemini-api files download env_xyz789 --output ./results`,
  },
  args: {
    ...globalFlags,
    "env-id": {
      type: "positional",
      description: "Environment ID",
      required: true,
    },
    output: {
      type: "string",
      description: "Output directory",
      default: "./",
    },
    "file-id": {
      type: "string",
      description: "Download specific file",
    },
  },
  async run({ args }) {
    try {
      const ctx = resolveContext(args);
      const envId = args["env-id"];
      const outputDir = args.output || "./";
      const fileId = args["file-id"];

      async function downloadFile(id: string, filePath?: string) {
        const url = `${ctx.baseUrl}/environments/${envId}/files/${id}`;
        const headers: Record<string, string> = {
          "x-goog-api-key": ctx.apiKey,
        };
        
        const response = await fetch(url, { headers });
        if (!response.ok) {
          throw new CLIError(`Failed to download file ${id}: ${response.statusText}`);
        }
        
        const arrayBuffer = await response.arrayBuffer();
        const buffer = Buffer.from(arrayBuffer);
        
        const filename = filePath ? path.basename(filePath) : id;
        const fullPath = path.join(outputDir, filename);
        
        fs.writeFileSync(fullPath, buffer);
        console.log(`  ${filePath || id} saved to ${fullPath}`);
      }

      if (args["dry-run"]) {
        const url = fileId 
          ? `/environments/${envId}/files/${fileId}`
          : `/environments/${envId}/files`;
        printCurl("GET", `${ctx.baseUrl}${url}`, ctx.apiKey);
        return;
      }

      if (!fs.existsSync(outputDir)) {
        fs.mkdirSync(outputDir, { recursive: true });
      }

      if (fileId) {
        await downloadFile(fileId);
      } else {
        // Download all
        const listUrl = `/environments/${envId}/files`;
        const response = await apiRequest<any>(ctx, "GET", listUrl);
        const files = Array.isArray(response) ? response : response.files || [];

        if (files.length === 0) {
          console.log(`No files found in environment ${envId}.`);
          return;
        }

        let downloadedCount = 0;
        for (const file of files) {
          const id = file.id || file.name; // Assume id or name
          if (id) {
            await downloadFile(id, file.path);
            downloadedCount++;
          }
        }
        console.log(`✓ Downloaded ${downloadedCount} files to ${outputDir}`);
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
