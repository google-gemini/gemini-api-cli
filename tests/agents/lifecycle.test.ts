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
    runCli(`agents init ${agentName} --base-agent waverunner`);
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
    // Add instructions to agent.yaml to see if it fixes 400
    const yamlPath = `${agentName}/agent.yaml`;
    const content = fs.readFileSync(yamlPath, "utf-8");
    fs.writeFileSync(yamlPath, content + "\ninstructions: You are a helpful assistant.\n");

    const result = runCli(`agents create --path ./${agentName}`);
    console.log("Create result:", result);
    expect(result).toContain("Created agent");
  });

  // Blocked: depends on create succeeding
  test.skip("list shows deployed agent — blocked: depends on create", () => {
    const result = runCli("agents list");
    expect(result).toContain(agentName);
  });

  // Blocked: depends on create succeeding
  test.skip("get shows agent details — blocked: depends on create", () => {
    const result = runCli(`agents get ${agentName} --json`);
    const agent = JSON.parse(result);
    expect(agent.id || agent.name).toContain(agentName);
  });

  // Blocked: depends on create succeeding
  test.skip("delete removes agent — blocked: depends on create", () => {
    const result = runCli(`agents delete ${agentName} --force`);
    expect(result).toContain("Deleted agent");
  });

  // Cleanup
  afterAll(() => {
    fs.rmSync(agentName, { recursive: true, force: true });
  });
});
