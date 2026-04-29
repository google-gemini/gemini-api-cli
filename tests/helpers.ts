import { execSync, ExecSyncOptions } from "node:child_process";

const CLI = "bun run src/cli.ts";

export function run(args: string, opts?: ExecSyncOptions): string {
  return execSync(`${CLI} ${args}`, {
    encoding: "utf-8",
    timeout: 120_000,  // 2 min timeout for API calls
    ...opts,
  });
}

export function runJson(args: string): any[] {
  const result = run(`${args} --json`);
  return result.trim().split("\n").map(l => JSON.parse(l));
}

export function runExpectError(args: string): string {
  try {
    execSync(`${CLI} ${args}`, { encoding: "utf-8", stdio: "pipe" });
    throw new Error("Expected command to fail");
  } catch (e: any) {
    return e.stderr || e.stdout || e.message;
  }
}

// Generate a 1x1 red pixel PNG for image tests
export function createTestPng(): Buffer {
  // Minimal valid PNG: 1x1 red pixel
  return Buffer.from(
    "89504e470d0a1a0a0000000d49484452000000010000000108020000009001" +
    "2e00000000c4944415408d763f8cfc0f0030001012718e3600000000049454e44ae426082",
    "hex"
  );
}
