// Plumbing smoke test: hits the mock server with both CLIs directly (no agent).
// Must use async exec — a sync exec would block the event loop and deadlock the
// in-process mock server.
import { spawn } from "node:child_process";
import path from "node:path";
import { MockGeminiServer } from "./mock-server.js";
import { evalRoot } from "./harnesses.js";

// stdin must be closed: the generated CLI blocks reading a piped stdin as a
// request body even on body-less GET commands (generator bug, see README).
function run(
  cmd: string,
  args: string[],
  opts: { env: NodeJS.ProcessEnv; timeout: number },
): Promise<{ stdout: string }> {
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
    child.on("close", (code) => {
      if (code === 0) resolve({ stdout });
      else reject(Object.assign(new Error(`exit ${code}`), { stdout, stderr }));
    });
    child.on("error", reject);
  });
}

async function main() {
  const s = new MockGeminiServer();
  const url = await s.start();
  const env = { ...process.env, GEMINI_API_KEY: "test-key", GEMINI_API_VERSION: "v1beta" };
  const gen = path.join(evalRoot, "bin", "generated", "gemini-api");
  const ref = path.join(evalRoot, "bin", "reference", "gemini-api-cli");

  try {
    const { stdout } = await run(
      gen,
      ["--server-url", url, "--no-interactive", "--output-format", "json", "agent", "list"],
      { env, timeout: 20000 },
    );
    console.log("GENERATED OK:", stdout.slice(0, 150).replace(/\n/g, " "));
  } catch (e: any) {
    console.log("GENERATED FAIL:", String(e.stdout || "").slice(0, 300), String(e.stderr || e).slice(0, 300));
  }
  try {
    const { stdout } = await run(ref, ["agent", "list", "--json"], {
      env: { ...env, GOOGLE_GEMINI_BASE_URL: url },
      timeout: 20000,
    });
    console.log("REFERENCE OK:", stdout.slice(0, 200).replace(/\n/g, " "));
  } catch (e: any) {
    console.log("REFERENCE FAIL:", String(e.stdout || "").slice(0, 300), String(e.stderr || e).slice(0, 300));
  }
  console.log("REQUESTS:", s.requests.map((r) => `${r.method} ${r.path}`).join(" | "));
  await s.stop();
}

main();
