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

import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import * as fs from "node:fs";
import * as path from "node:path";
import { collectInlineFiles } from "../src/lib/files";

describe("collectInlineFiles filtering", () => {
  const testDir = path.join(__dirname, "tmp-test-agent");

  beforeAll(() => {
    // Setup test directory structure
    fs.mkdirSync(testDir, { recursive: true });
    fs.mkdirSync(path.join(testDir, "workspace"), { recursive: true });
    fs.mkdirSync(path.join(testDir, "skills"), { recursive: true });
    fs.mkdirSync(path.join(testDir, "ignored_dir"), { recursive: true });

    // Allowed files
    fs.writeFileSync(path.join(testDir, "AGENTS.md"), "agents content");

    fs.writeFileSync(path.join(testDir, "workspace", "file1.txt"), "file1 content");
    fs.writeFileSync(path.join(testDir, "skills", "skill1.js"), "skill1 content");

    // Ignored files
    fs.writeFileSync(path.join(testDir, "agent.yaml"), "agent config");
    fs.writeFileSync(path.join(testDir, "ignored_file.txt"), "ignored file");
    fs.writeFileSync(path.join(testDir, "ignored_dir", "file2.txt"), "ignored dir file");
    fs.writeFileSync(path.join(testDir, "package.json"), "{}");
  });

  afterAll(() => {
    // Cleanup
    fs.rmSync(testDir, { recursive: true, force: true });
  });

  test("should only collect allowed files", async () => {
    const files = await collectInlineFiles(testDir);

    const targets = files.map((f) => f.target);
    console.log("Collected targets:", targets);


    expect(targets).toContain("/.agents/AGENTS.md");
    expect(targets).toContain("/.agents/workspace/file1.txt");
    expect(targets).toContain("/.agents/skills/skill1.js");

    // Should NOT contain ignored files
    expect(targets).not.toContain("/.agents/agent.yaml");
    expect(targets).not.toContain("/.agents/ignored_file.txt");
    expect(targets).not.toContain("/.agents/ignored_dir/file2.txt");
    expect(targets).not.toContain("/.agents/package.json");

    // Total expected allowed files is 3
    expect(files.length).toBe(3);
  });
});
