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

import { describe, test, expect, afterAll } from "bun:test";
import { execSync } from "child_process";
import * as fs from "node:fs";

describe("agents lifecycle", () => {
  const agentName = `test-agent-${Date.now()}`;

  // Helper to run CLI
  const runCli = (args: string) => {
    const cmd = `bun run src/cli.ts ${args} 2>&1`;
    try {
      return execSync(cmd, { encoding: "utf-8" });
    } catch (e: any) {
      return e.stdout;
    }
  };

  test("init creates directory", () => {
    runCli(`agents init ${agentName} --base-agent antigravity-preview-05-2026`);
    expect(fs.existsSync(`${agentName}/agent.yaml`)).toBe(true);
    expect(fs.existsSync(`${agentName}/AGENTS.md`)).toBe(true);
    expect(fs.existsSync(`${agentName}/skills`)).toBe(true);
    expect(fs.existsSync(`${agentName}/.env`)).toBe(true);
  });

  test("init is idempotent", () => {
    const result = runCli(`agents init ${agentName}`);
    expect(result).toContain("already exists");
  });

  test("create deploys agent", () => {
    const result = runCli(`agents create --path ./${agentName}`);
    console.log("Create result:", result);
    expect(result).toContain("Created agent");
  });

  // Blocked: depends on create succeeding
  test("list shows deployed agent", () => {
    const result = runCli("agents list");
    expect(result).toContain(agentName);
  });

  // Blocked: depends on create succeeding
  test("get shows agent details", () => {
    const result = runCli(`agents get ${agentName} --json`);
    const agent = JSON.parse(result);
    expect(agent.id || agent.name).toContain(agentName);
  });

  // Blocked: depends on create succeeding
  test("delete removes agent", () => {
    const result = runCli(`agents delete ${agentName} --force`);
    expect(result).toContain("Deleted agent");
  });

  // Cleanup
  afterAll(() => {
    fs.rmSync(agentName, { recursive: true, force: true });
  });
});
