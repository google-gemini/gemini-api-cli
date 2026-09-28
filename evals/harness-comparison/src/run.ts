import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { MockGeminiServer } from "./mock-server.js";
import { scenarios, type Scenario } from "./scenarios.js";
import { harnesses, systemPrompt, evalRoot, type Harness } from "./harnesses.js";
import { prComment, summarize, type RunResult } from "./report.js";

function parseArgs() {
  const args = process.argv.slice(2);
  const get = (flag: string, dflt: string) => {
    const i = args.indexOf(flag);
    return i >= 0 && args[i + 1] ? args[i + 1] : dflt;
  };
  return {
    harnesses: get("--harnesses", harnesses.map((h) => h.id).join(",")).split(","),
    scenarios: get("--scenarios", scenarios.map((s) => s.id).join(",")).split(","),
    repeats: parseInt(get("--repeats", "1"), 10),
    maxTurns: parseInt(get("--max-turns", "12"), 10),
    caseTimeoutSeconds: parseInt(get("--case-timeout-seconds", "60"), 10),
    model: get("--model", "") || undefined,
    commentOut: get("--comment-out", "") || undefined,
  };
}

function buildEnv(h: Harness, baseUrl: string, tmpConfig: string): Record<string, string> {
  const env: Record<string, string> = {};
  for (const [k, v] of Object.entries(process.env)) {
    if (v === undefined) continue;
    // Isolate from any real Gemini/Google credentials or config on this machine.
    if (k.startsWith("GEMINI_") || k.startsWith("GOOGLE_")) continue;
    env[k] = v;
  }
  env.PATH = `${h.binDir}:${env.PATH ?? ""}`;
  // Fake key for the CLI under test. The driver's own Gemini auth is passed
  // separately as LocalAgentConfig.api_key, so this never collides with it.
  env.GEMINI_API_KEY = "eval-test-key";
  env.XDG_CONFIG_HOME = tmpConfig;
  env.GEMINI_CONFIG_DIR = path.join(tmpConfig, "gemini-api-cli");
  Object.assign(env, h.env(baseUrl));
  return env;
}

async function runOne(
  h: Harness,
  s: Scenario,
  repeat: number,
  maxTurns: number,
  caseTimeoutMs: number,
  model: string | undefined,
): Promise<RunResult> {
  const server = new MockGeminiServer();
  const baseUrl = await server.start();
  const tmpConfig = fs.mkdtempSync(path.join(os.tmpdir(), "harness-eval-"));
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), "harness-eval-cwd-"));
  const started = Date.now();

  let assistantTurns = 0;
  let bashCalls = 0;
  let apiCallMade = false;
  let bashCallsToApiCall: number | null = null;
  let assistantTurnsToApiCall: number | null = null;
  let resultSubtype = "unknown";
  let finalReply = "";
  let costUsd: number | null = null;
  let numTurns = 0;
  const bashCommands: string[] = [];

  const checkMatch = () => {
    if (apiCallMade) return;
    if (server.requests.some((r) => s.expect(r))) {
      apiCallMade = true;
      bashCallsToApiCall = bashCalls;
      assistantTurnsToApiCall = assistantTurns;
    }
  };

  let modelUsed = model ?? "";
  let totalTokens: number | null = null;

  const apiKey = process.env.EVAL_GEMINI_API_KEY;
  if (!apiKey) throw new Error("EVAL_GEMINI_API_KEY is required (Antigravity driver auth)");

  const driverCfgPath = path.join(tmpConfig, "driver-config.json");
  fs.writeFileSync(
    driverCfgPath,
    JSON.stringify({
      task: s.task,
      system: systemPrompt(h, baseUrl),
      cwd,
      env: buildEnv(h, baseUrl, tmpConfig),
      model: model ?? null,
      maxToolCalls: maxTurns,
      apiKey,
    }),
  );

  try {
    const python =
      process.env.EVAL_PYTHON ?? path.join(evalRoot, ".venv", "bin", "python3");
    const driver = path.join(evalRoot, "driver", "antigravity_driver.py");
    await new Promise<void>((resolve, reject) => {
      const useProcessGroup = process.platform !== "win32";
      const child = spawn(python, [driver, driverCfgPath], {
        stdio: ["ignore", "pipe", "pipe"],
        detached: useProcessGroup,
      });
      let timedOut = false;
      let killTimer: NodeJS.Timeout | undefined;
      const killChild = (signal: NodeJS.Signals) => {
        if (child.pid === undefined) return;
        try {
          process.kill(useProcessGroup ? -child.pid : child.pid, signal);
        } catch (err) {
          const code = (err as NodeJS.ErrnoException).code;
          if (code === "ESRCH") return;
          // If group signalling is unavailable, still terminate the driver.
          try {
            child.kill(signal);
          } catch {
            // The close handler will record the timeout even if the process
            // disappeared between the two signalling attempts.
          }
        }
      };
      const timeoutTimer = setTimeout(() => {
        timedOut = true;
        killChild("SIGTERM");
        killTimer = setTimeout(() => killChild("SIGKILL"), 2_000);
        killTimer.unref();
      }, caseTimeoutMs);
      timeoutTimer.unref();
      let buffered = "";
      let stderr = "";
      child.stderr.on("data", (d) => (stderr += d));
      child.stdout.on("data", (d) => {
        buffered += d;
        let idx;
        while ((idx = buffered.indexOf("\n")) >= 0) {
          const line = buffered.slice(0, idx);
          buffered = buffered.slice(idx + 1);
          if (!line.trim()) continue;
          let event: Record<string, unknown>;
          try {
            event = JSON.parse(line);
          } catch {
            continue;
          }
          if (event.type === "tool_call") {
            bashCalls++;
            assistantTurns++;
            bashCommands.push(String(event.command ?? ""));
          }
          if (event.type === "result") {
            finalReply = String(event.text ?? "");
            numTurns = Number(event.turns ?? 0);
            modelUsed = String(event.model ?? modelUsed);
            const usage = event.usage as { total_token_count?: number } | null;
            totalTokens = usage?.total_token_count ?? null;
            resultSubtype = event.error ? `driver error: ${event.error}` : "success";
          }
          // Requests land on the mock while tools execute; re-check per event
          // so turn counters reflect when the call first became visible.
          checkMatch();
        }
      });
      child.on("close", (code) => {
        clearTimeout(timeoutTimer);
        if (killTimer) clearTimeout(killTimer);
        if (timedOut) {
          reject(new Error(`timed out after ${(caseTimeoutMs / 1000).toFixed(0)}s`));
          return;
        }
        if (code === 0 || resultSubtype !== "unknown") resolve();
        else reject(new Error(`driver exit ${code}: ${stderr.slice(-400)}`));
      });
      child.on("error", (err) => {
        clearTimeout(timeoutTimer);
        if (killTimer) clearTimeout(killTimer);
        reject(err);
      });
    });
  } catch (err) {
    resultSubtype = `exception: ${err instanceof Error ? err.message : String(err)}`;
  }
  checkMatch();
  await server.stop();

  return {
    harness: h.id,
    scenario: s.id,
    repeat,
    apiCallMade,
    bashCallsToApiCall,
    assistantTurnsToApiCall,
    totalBashCalls: bashCalls,
    numTurns,
    resultSubtype,
    finalReply: finalReply.slice(0, 300),
    costUsd,
    model: modelUsed || null,
    totalTokens,
    durationMs: Date.now() - started,
    bashCommands,
    requestLog: server.requests.map((r) => `${r.method} ${r.path}${r.body ? ` ${r.body.slice(0, 120)}` : ""}`),
  };
}

async function main() {
  const opts = parseArgs();
  if (!Number.isFinite(opts.caseTimeoutSeconds) || opts.caseTimeoutSeconds <= 0) {
    throw new Error("--case-timeout-seconds must be a positive number");
  }
  const hs = harnesses.filter((h) => opts.harnesses.includes(h.id));
  const ss = scenarios.filter((s) => opts.scenarios.includes(s.id));
  if (!hs.length || !ss.length) {
    console.error("No matching harnesses/scenarios. Available:");
    console.error(`  harnesses: ${harnesses.map((h) => h.id).join(", ")}`);
    console.error(`  scenarios: ${scenarios.map((s) => s.id).join(", ")}`);
    process.exit(1);
  }

  const outDir = path.join(evalRoot, "results");
  fs.mkdirSync(outDir, { recursive: true });
  const stamp = new Date().toISOString().replace(/[:.]/g, "-");
  const jsonPath = path.join(outDir, `run-${stamp}.json`);
  const summaryPath = path.join(outDir, `run-${stamp}.md`);
  const commentPath = path.join(outDir, `run-${stamp}-comment.md`);
  const results: RunResult[] = [];
  const persistResults = () => {
    fs.writeFileSync(jsonPath, JSON.stringify(results, null, 2));
    fs.writeFileSync(summaryPath, summarize(results));
    if (results.length === 0) return;
    const comment = prComment(results, opts.model);
    fs.writeFileSync(commentPath, comment);
    if (opts.commentOut) fs.writeFileSync(opts.commentOut, comment);
  };
  persistResults();
  for (let rep = 0; rep < opts.repeats; rep++) {
    for (const s of ss) {
      for (const h of hs) {
        process.stderr.write(`[${h.id} / ${s.id} / rep ${rep}] running...\n`);
        const r = await runOne(
          h,
          s,
          rep,
          opts.maxTurns,
          opts.caseTimeoutSeconds * 1_000,
          opts.model,
        );
        process.stderr.write(
          `[${h.id} / ${s.id} / rep ${rep}] api=${r.apiCallMade} bashToCall=${r.bashCallsToApiCall} total=${r.totalBashCalls} ${(
            r.durationMs / 1000
          ).toFixed(1)}s\n`,
        );
        results.push(r);
        persistResults();
      }
    }
  }

  const summary = summarize(results);
  console.log(summary);
  console.log(`\nFull results: ${jsonPath}`);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
