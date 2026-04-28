import { describe, test, expect } from "bun:test";
import { spawnSync, execSync } from "child_process";
import * as fs from "fs";

describe("output modes", () => {
  const runCli = (args: string[], input?: string) => {
    const res = spawnSync("bun", ["run", "src/cli.ts", ...args], {
      encoding: "utf-8",
      input: input,
    });
    return res;
  };

  test("human mode outputs text to stdout and summary to stderr", () => {
    if (!process.env.GEMINI_API_KEY) {
        console.warn("Skipping live API test because GEMINI_API_KEY is not set");
        return;
    }
    const res = runCli(["run", "Say exactly: output-test", "--no-stream"]);
    expect(res.stdout).toContain("output-test");
    expect(res.stderr).toContain("✓ completed");
    expect(res.stderr).toContain("interaction_id:");
  });

  test("json mode outputs valid JSONL to stdout", () => {
    if (!process.env.GEMINI_API_KEY) return;
    const res = runCli(["run", "Say hi", "--json"]);
    const lines = res.stdout.trim().split("\n");
    expect(lines.length).toBeGreaterThan(0);
    for (const line of lines) {
      expect(() => JSON.parse(line)).not.toThrow();
    }
    const events = lines.map(l => JSON.parse(l));
    expect(events.some(e => e.event_type === "interaction.start")).toBe(true);
    expect(events.some(e => e.event_type === "interaction.complete")).toBe(true);
    // Verify summary is NOT in stderr or stdout
    expect(res.stderr).not.toContain("✓ completed");
  });


  test("errors include Try: with correct invocation", () => {
    const res = runCli(["run"]);
    expect(res.stderr).toContain("✗ Missing prompt");
    expect(res.stderr).toContain("Try:");
    expect(res.stderr).toContain("gemini-api run");
  });

  test('redirection captures only response text', () => {
    if (!process.env.GEMINI_API_KEY) return;
    if (!fs.existsSync("tmp")) fs.mkdirSync("tmp");
    
    const cmd = `bun run src/cli.ts run "Say exactly: hello-redirection" --no-stream > tmp/out.txt`;
    execSync(cmd);
    
    const content = fs.readFileSync("tmp/out.txt", "utf-8");
    expect(content).toContain("hello-redirection");
    expect(content).not.toContain("✓ completed");
    
    fs.unlinkSync("tmp/out.txt");
  });

  test('json redirection captures raw events', () => {
    if (!process.env.GEMINI_API_KEY) return;
    if (!fs.existsSync("tmp")) fs.mkdirSync("tmp");
    
    const cmd = `bun run src/cli.ts run "Say hi" --json > tmp/out.jsonl`;
    execSync(cmd);
    
    const content = fs.readFileSync("tmp/out.jsonl", "utf-8");
    const lines = content.trim().split("\n");
    expect(lines.length).toBeGreaterThan(0);
    expect(() => JSON.parse(lines[0])).not.toThrow();
    
    fs.unlinkSync("tmp/out.jsonl");
  }, 10000);
});

describe("dry-run", () => {
  const runCli = (args: string[]) => {
    const res = spawnSync("bun", ["run", "src/cli.ts", ...args], {
      encoding: "utf-8",
    });
    return res;
  };

  test("run --dry-run prints curl", () => {
    const res = runCli(["run", "Hello", "--dry-run", "--api-key", "fake-key"]);
    expect(res.stdout).toContain("curl -X POST");
    expect(res.stdout).toContain("fake-key");
    expect(res.stdout).toContain("https://generativelanguage.googleapis.com");
  });

  test("agents create --dry-run prints curl", () => {
    spawnSync("bun", ["run", "src/cli.ts", "agents", "init", "dry-run-test"], { encoding: "utf-8" });
    const res = runCli(["agents", "create", "--path", "./dry-run-test", "--dry-run", "--api-key", "fake-key"]);
    expect(res.stdout).toContain("curl -X POST");
    expect(res.stdout).toContain("/agents");
    fs.rmSync("dry-run-test", { recursive: true, force: true });
  });

  test("agents list --dry-run prints curl", () => {
    const res = runCli(["agents", "list", "--dry-run", "--api-key", "fake-key"]);
    expect(res.stdout).toContain("curl -X GET");
    expect(res.stdout).toContain("/agents");
  });

  test("agents delete --dry-run prints curl", () => {
    const res = runCli(["agents", "delete", "test-id", "--dry-run", "--api-key", "fake-key"]);
    expect(res.stdout).toContain("curl -X DELETE");
    expect(res.stdout).toContain("/agents/test-id");
  });

  test("files list --dry-run prints curl", () => {
    const res = runCli(["files", "list", "env_fake", "--dry-run", "--api-key", "fake-key"]);
    expect(res.stdout).toContain("curl");
  });
});
