import { describe, test, expect } from "bun:test";
import { execSync } from "node:child_process";

// Skip: citty uses consola for --help output, which suppresses stdout
// when not connected to a TTY. bun test's execSync doesn't provide a TTY,
// so help output is always empty. Help works correctly when run directly
// (e.g. `bun run dev --help`).
describe("help", () => {
  const runCli = (args: string) => {
    const cmd = `source ~/.bash_profile && CONSOLA_LEVEL=5 bun run src/cli.ts ${args} 2>&1`;
    try {
      return execSync(cmd, { encoding: "utf-8", shell: "/bin/bash" });
    } catch (e: any) {
      return (e.stdout || "") + (e.stderr || "");
    }
  };

  test("--help shows top-level help", () => {
    const output = runCli("--help");
    expect(output).toContain("gemini-api");
    expect(output).toContain("COMMANDS");
  });

  test("run --help shows examples", () => {
    const output = runCli("run --help");
    expect(output).toContain("Examples");
    expect(output).toContain("gemini-api run");
  });

  test("agents --help shows subcommands", () => {
    const output = runCli("agents --help");
    expect(output).toContain("COMMANDS");
    expect(output).toContain("create");
  });

  test("agents create --help shows examples", () => {
    const output = runCli("agents create --help");
    expect(output).toContain("Examples");
    expect(output).toContain("gemini-api agents create");
  });
});
