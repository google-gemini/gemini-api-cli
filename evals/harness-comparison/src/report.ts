import { scenarios } from "./scenarios.js";

export interface RunResult {
  harness: string;
  scenario: string;
  repeat: number;
  /** The correct API call was observed on the mock server. */
  apiCallMade: boolean;
  /** Bash invocations issued before (and including) the one that made the call. */
  bashCallsToApiCall: number | null;
  /** Assistant messages seen before the call was observed. */
  assistantTurnsToApiCall: number | null;
  totalBashCalls: number;
  numTurns: number;
  resultSubtype: string;
  finalReply: string;
  costUsd: number | null;
  model?: string | null;
  totalTokens?: number | null;
  durationMs: number;
  bashCommands: string[];
  requestLog: string[];
}

export /**
 * PR-comment markdown: one row per scenario prompt, one column pair per
 * harness (success + Bash calls until the correct API call was made).
 */
function prComment(results: RunResult[], model: string | undefined): string {
  const hids = [...new Set(results.map((r) => r.harness))];
  const sids = [...new Set(results.map((r) => r.scenario))];
  const lines: string[] = [];
  lines.push("### CLI harness eval");
  lines.push("");
  lines.push(
    "An Antigravity agent (shell-only) is asked to perform each task with the CLI; " +
      "“turns” is the number of shell invocations until the correct API call hit the mock server.",
  );
  lines.push("");
  lines.push(`| Prompt | ${hids.map((h) => `${h} | turns`).join(" | ")} |`);
  lines.push(`|---|${hids.map(() => "---|---").join("|")}|`);
  for (const sid of sids) {
    const scenario = scenarios.find((s) => s.id === sid);
    const prompt = (scenario?.task ?? sid).replace(/\|/g, "\\|");
    const cells = hids.map((hid) => {
      const rs = results.filter((r) => r.harness === hid && r.scenario === sid);
      if (!rs.length) return "— | —";
      const ok = rs.filter((r) => r.apiCallMade).length;
      const status = ok === rs.length ? "✅" : ok === 0 ? "❌" : `${ok}/${rs.length}`;
      const withCall = rs.filter((r) => r.bashCallsToApiCall !== null);
      const turns = withCall.length
        ? (withCall.reduce((a, r) => a + (r.bashCallsToApiCall ?? 0), 0) / withCall.length).toFixed(
            rs.length > 1 ? 1 : 0,
          )
        : "—";
      return `${status} | ${turns}`;
    });
    lines.push(`| ${prompt} | ${cells.join(" | ")} |`);
  }
  lines.push("");
  const aggregates = hids.map((hid) => {
    const rs = results.filter((r) => r.harness === hid);
    const ok = rs.filter((r) => r.apiCallMade);
    const avg = ok.length
      ? (ok.reduce((a, r) => a + (r.bashCallsToApiCall ?? 0), 0) / ok.length).toFixed(1)
      : "—";
    return `**${hid}**: ${ok.length}/${rs.length} succeeded, avg ${avg} turns to API call`;
  });
  lines.push(aggregates.join(" · "));
  const models = [...new Set(results.map((r) => r.model).filter(Boolean))] as string[];
  const modelLabel = models.length ? models.join(", ") : model ?? "unknown";
  const tokens = results.reduce((a, r) => a + (r.totalTokens ?? 0), 0);
  const cost = results.reduce((a, r) => a + (r.costUsd ?? 0), 0);
  lines.push("");
  const costPart = cost > 0 ? `, total cost $${cost.toFixed(2)}` : "";
  const tokenPart = tokens > 0 ? `, ${(tokens / 1000).toFixed(0)}k tokens` : "";
  lines.push(
    `<sub>Driver: Antigravity SDK, model ${modelLabel} · ${results.length} agent sessions${tokenPart}${costPart}.</sub>`,
  );
  return lines.join("\n");
}

export function summarize(results: RunResult[]): string {
  const lines: string[] = [];
  lines.push("| harness | scenario | api call | bash calls to call | total bash | turns | duration | result |");
  lines.push("|---|---|---|---|---|---|---|---|");
  for (const r of results) {
    lines.push(
      `| ${r.harness} | ${r.scenario} | ${r.apiCallMade ? "✅" : "❌"} | ${r.bashCallsToApiCall ?? "—"} | ${r.totalBashCalls} | ${r.numTurns} | ${(r.durationMs / 1000).toFixed(1)}s | ${r.resultSubtype} |`,
    );
  }
  // Per-harness aggregate
  lines.push("");
  lines.push("| harness | success rate | avg bash calls to call | avg duration |");
  lines.push("|---|---|---|---|");
  for (const hid of new Set(results.map((r) => r.harness))) {
    const rs = results.filter((r) => r.harness === hid);
    const ok = rs.filter((r) => r.apiCallMade);
    const avgCalls = ok.length
      ? (ok.reduce((a, r) => a + (r.bashCallsToApiCall ?? 0), 0) / ok.length).toFixed(1)
      : "—";
    const avgDur = (rs.reduce((a, r) => a + r.durationMs, 0) / rs.length / 1000).toFixed(1);
    lines.push(`| ${hid} | ${ok.length}/${rs.length} | ${avgCalls} | ${avgDur}s |`);
  }
  return lines.join("\n");
}
