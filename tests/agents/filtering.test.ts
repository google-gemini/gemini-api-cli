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
import { execSync } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";

describe("agents create inlining integration", () => {
  const agentName = `test-filter-agent-${Date.now()}`;

  beforeAll(() => {
    // 1. Initialize agent
    const initCmd = `bun run src/cli.ts agents init ${agentName} --base-agent antigravity-preview-05-2026`;
    execSync(initCmd, { encoding: "utf-8" });

    // 2. Add some extra files that should be ignored
    fs.writeFileSync(path.join(agentName, "ignored_at_root.txt"), "ignore me");
    fs.mkdirSync(path.join(agentName, "ignored_dir"), { recursive: true });
    fs.writeFileSync(path.join(agentName, "ignored_dir", "file.txt"), "ignore me too");

    // 3. Add some allowed files
    fs.writeFileSync(path.join(agentName, "workspace", "allowed.txt"), "keep me");
    fs.mkdirSync(path.join(agentName, "skills", "sub"), { recursive: true });
    fs.writeFileSync(path.join(agentName, "skills", "sub", "allowed_skill.js"), "keep me too");
  });

  afterAll(() => {
    fs.rmSync(agentName, { recursive: true, force: true });
  });

  test("should only include allowed files in dry-run curl output", () => {
    const createCmd = `bun run src/cli.ts agents create --path ./${agentName} --dry-run`;
    const stdout = execSync(createCmd, { encoding: "utf-8" });

    const startIndex = stdout.indexOf("{");
    const endIndex = stdout.lastIndexOf("}");
    expect(startIndex).not.toBe(-1);
    expect(endIndex).not.toBe(-1);

    const rawBodyStr = stdout.substring(startIndex, endIndex + 1);

    // Revert bash escaping of single quotes: '\'' -> '
    // In the raw string, this is represented as '\n  ... agent'\\''s ...'
    // We need to replace matches of: ' (single quote) followed by \' (escaped quote in JS, which is \\' in regex) followed by '
    // Actually, printCurl did: replace(/'/g, "'\\''")
    // So we just need to replace "'\\''" with "'"
    const bodyStr = rawBodyStr.replace(/'\\''/g, "'");

    const body = JSON.parse(bodyStr);

    expect(body.base_environment).toBeDefined();
    expect(body.base_environment.type).toBe("remote");
    expect(body.base_environment.sources).toBeDefined();

    const sources = body.base_environment.sources;
    const targets = sources.map((s: any) => s.target);
    console.log("Inlined targets in dry-run:", targets);

    // Allowed
    expect(targets).toContain("/credentials/.env");
    expect(targets).toContain("/.agents/AGENTS.md");
    expect(targets).toContain("/.agents/workspace/allowed.txt");
    expect(targets).toContain("/.agents/skills/sub/allowed_skill.js");

    // Ignored
    expect(targets).not.toContain("/.agents/agent.yaml");
    expect(targets).not.toContain("/.agents/ignored_at_root.txt");
    expect(targets).not.toContain("/.agents/ignored_dir/file.txt");

    // Total expected sources is 4
    expect(sources.length).toBe(4);
  });
});
