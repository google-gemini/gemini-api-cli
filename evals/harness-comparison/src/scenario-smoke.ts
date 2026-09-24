// Deterministic scenario smoke: drives the GENERATED CLI directly (no agent)
// through one canonical invocation per scenario and asserts the captured
// request satisfies the scenario's expect(). Catches drift between the CLI
// surface and the eval scenarios without spending agent tokens.
import { spawn } from "node:child_process";
import path from "node:path";
import { MockGeminiServer } from "./mock-server.js";
import { evalRoot } from "./harnesses.js";
import { scenarios } from "./scenarios.js";

function run(
  cmd: string,
  args: string[],
  opts: { env: NodeJS.ProcessEnv; timeout: number },
): Promise<{ code: number; stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    const child = spawn(cmd, args, {
      env: opts.env,
      stdio: ["ignore", "pipe", "pipe"],
      timeout: opts.timeout,
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (d) => (stdout += d));
    child.stderr.on("data", (d) => (stderr += d));
    child.on("close", (code) => resolve({ code: code ?? 1, stdout, stderr }));
    child.on("error", reject);
  });
}

// One canonical generated-CLI invocation per scenario id.
const invocations: Record<string, string[]> = {
  "create-agent": [
    "agent",
    "create",
    "--body",
    '{"id":"demo-agent","base_agent":"deep-research","system_instruction":"You are a helpful test agent."}',
  ],
  "list-agents": ["agent", "list"],
  "get-agent": ["agent", "get", "--id", "demo-agent"],
  "delete-agent": ["agent", "delete", "--id", "demo-agent"],
  "run-agent-background": [
    "agent",
    "run",
    "--body",
    '{"agent":"demo-agent","input":"say hello","background":true}',
  ],
  "interaction-status": ["agent", "status", "--id", "int-123"],
  "cancel-interaction": ["agent", "cancel", "--id", "int-123"],
  "generate-default-model": ["generate", "What is concurrency?"],
  "generate-image": ["image", "a red circle on a white background"],
  "generate-music": ["music", "an upbeat jingle"],
  "generate-video-background": ["video", "a bouncing ball"],
  "list-models-live": ["models", "list"],
};

async function main() {
  const cli = path.join(evalRoot, "bin", "generated", "gemini-api");
  let failures = 0;
  for (const scenario of scenarios) {
    const args = invocations[scenario.id];
    if (!args) {
      console.log(`SKIP  ${scenario.id} (no canonical invocation)`);
      continue;
    }
    const server = new MockGeminiServer();
    const url = await server.start();
    const env = {
      ...process.env,
      GEMINI_API_KEY: "test-key",
      GEMINI_API_VERSION: "v1beta",
    };
    const full = [
      "--server-url",
      url,
      "--no-interactive",
      "--no-retries",
      "--output-format",
      "json",
      "--color",
      "never",
      ...args,
    ];
    const result = await run(cli, full, { env, timeout: 30_000 });
    await server.stop();
    const satisfied = server.requests.some((r) => scenario.expect(r));
    if (!satisfied) {
      failures++;
      console.log(`FAIL  ${scenario.id} (exit ${result.code})`);
      console.log(
        `      requests: ${server.requests.map((r) => `${r.method} ${r.path}`).join(", ") || "none"}`,
      );
      if (result.stderr)
        console.log(`      stderr: ${result.stderr.slice(0, 300)}`);
    } else {
      console.log(`ok    ${scenario.id}`);
    }
  }
  if (failures > 0) {
    console.error(`\n${failures} scenario(s) failed`);
    process.exit(1);
  }
  console.log(
    `\nall ${scenarios.length} scenarios satisfied by the generated CLI`,
  );
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
