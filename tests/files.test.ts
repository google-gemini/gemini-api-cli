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

import { describe, test, expect } from "bun:test";
import { execSync } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";

describe("files (live API)", () => {
  test("download environment snapshot", async () => {
    if (!process.env.GEMINI_API_KEY) {
      console.warn("Skipping live API test because GEMINI_API_KEY is not set");
      return;
    }

    // 1. Create a file in the environment
    const runCmd = `source ~/.bash_profile && bun run src/cli.ts run "Write 'hello world' to /tmp/test.txt" --agent waverunner 2>&1`;
    let output = "";
    try {
      output = execSync(runCmd, { encoding: "utf-8", shell: "/bin/bash" });
    } catch (e: any) {
      output = e.stdout || e.stderr || "";
    }

    console.log("Run output:", output);

    // 2. Extract environment_id
    const envIdMatch = output.match(/environment_id:\s*([a-zA-Z0-9-]+)/);
    if (!envIdMatch) {
      throw new Error("Failed to extract environment_id from output");
    }
    const envId = envIdMatch[1];
    console.log(`Found environment_id: ${envId}`);

    // 3. Download the snapshot
    const downloadCmd = `source ~/.bash_profile && bun run src/cli.ts files download ${envId} --output ./tmp 2>&1`;
    let downloadOutput = "";
    try {
      downloadOutput = execSync(downloadCmd, { encoding: "utf-8", shell: "/bin/bash" });
    } catch (e: any) {
      downloadOutput = e.stdout || e.stderr || "";
    }

    console.log("Download output:", downloadOutput);

    // 4. Verify
    // The user wants us to run it. If it fails with 404, the test will fail,
    // which is what we expect if the endpoint is broken as the comment said.
    // But let's assert what we expect on success.
    expect(downloadOutput).toContain("Saved snapshot");
    
    // Cleanup
    const snapshotDir = path.join("./tmp", `snapshot_${envId}`);
    if (fs.existsSync(snapshotDir)) {
      fs.rmSync(snapshotDir, { recursive: true, force: true });
    }
  }, 60000); // Give it 60 seconds as it involves interaction + download
});
