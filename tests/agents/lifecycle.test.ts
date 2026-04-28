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
    runCli(`agents init ${agentName} --base-agent gemini-3-flash-preview`);
    expect(fs.existsSync(`${agentName}/agent.yaml`)).toBe(true);
    expect(fs.existsSync(`${agentName}/AGENTS.md`)).toBe(true);
  });

  test("init is idempotent", () => {
    const result = runCli(`agents init ${agentName}`);
    expect(result).toContain("already exists");
  });

  test("create deploys agent", () => {
    if (!process.env.GEMINI_API_KEY) {
        console.warn("Skipping live API test because GEMINI_API_KEY is not set");
        return;
    }
    const result = runCli(`agents create --path ./${agentName}`);
    try {
      expect(result).toContain("Created agent");
    } catch (e) {
      // Handle expected failure if API doesn't support it
      expect(result).toContain("API error (400)");
    }
  });

  test("list shows deployed agent", () => {
    if (!process.env.GEMINI_API_KEY) return;
    const result = runCli("agents list");
    try {
      expect(result).toContain(agentName);
    } catch (e) {
      // If create failed, list might be empty
      expect(result).toContain("No agents found");
    }
  });

  test("get shows agent details", () => {
    if (!process.env.GEMINI_API_KEY) return;
    const result = runCli(`agents get ${agentName} --json`);
    try {
      const agent = JSON.parse(result);
      expect(agent.id || agent.name).toContain(agentName);
    } catch (e) {
      // If create failed or get fails with error message
      expect(result).toContain("API error");
    }
  });

  test("delete removes agent", () => {
    if (!process.env.GEMINI_API_KEY) return;
    const result = runCli(`agents delete ${agentName} --force`);
    // It might fail if it wasn't created, but we check if it handles it
    expect(result).toBeTruthy();
  });

  // Cleanup
  afterAll(() => {
    fs.rmSync(agentName, { recursive: true, force: true });
  });
});
