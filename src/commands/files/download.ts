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
import { resolveContext, apiRequest } from "../../lib/api";
import { printCurl, printError } from "../../lib/output";
import { CLIError } from "../../lib/errors";
import * as fs from "node:fs";
import * as path from "node:path";

export default defineCommand({
  meta: {
    name: "download",
    description: `Download files from environment as a snapshot and extract them.
 
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
  },
  async run({ args }) {
    try {
      const ctx = resolveContext(args);
      const envId = args["env-id"];
      const outputDir = args.output || "./";

      const url = `/files/environment-${envId}:download?alt=media`;

      if (args["dry-run"]) {
        printCurl("GET", `${ctx.baseUrl}${url}`, ctx.apiKey);
        return;
      }

      if (!fs.existsSync(outputDir)) {
        fs.mkdirSync(outputDir, { recursive: true });
      }

      const fullUrl = `${ctx.baseUrl}${url}`;
      const headers: Record<string, string> = {
        "x-goog-api-key": ctx.apiKey,
      };

      console.log(`Downloading snapshot for environment ${envId}...`);
      const response = await fetch(fullUrl, { headers });
      
      if (!response.ok) {
        throw new CLIError(`Failed to download snapshot: ${response.statusText}`);
      }

      const arrayBuffer = await response.arrayBuffer();
      const buffer = Buffer.from(arrayBuffer);
      
      const snapshotPath = path.join(outputDir, `snapshot_${envId}.tar`);
      fs.writeFileSync(snapshotPath, buffer);
      console.log(`✓ Saved snapshot to ${snapshotPath}`);

      // Extract it
      const extractDir = path.join(outputDir, `snapshot_${envId}`);
      if (!fs.existsSync(extractDir)) {
        fs.mkdirSync(extractDir, { recursive: true });
      }

      console.log(`Extracting snapshot to ${extractDir}...`);
      try {
        const { execSync } = await import("node:child_process");
        execSync(`tar xf ${snapshotPath} -C ${extractDir}`);
        console.log(`✓ Extracted snapshot to ${extractDir}`);
        fs.unlinkSync(snapshotPath);
      } catch (error) {
        console.error(`✗ Failed to extract snapshot: ${(error as Error).message}`);
        console.log(`Snapshot file is still available at ${snapshotPath}`);
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
