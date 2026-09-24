import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
export const evalRoot = path.resolve(here, "..");

export interface Harness {
  id: string;
  cliName: string;
  /** Directory prepended to PATH containing only this harness's binary. */
  binDir: string;
  /**
   * One sentence of plumbing so the CLI reaches the mock server. Each harness
   * gets its own affordance (flag vs env var) — this is test plumbing, not a
   * usage hint, and is the minimum required for the CLI to work at all.
   */
  plumbing: (baseUrl: string) => string;
  /** Extra env vars for the agent process. */
  env: (baseUrl: string) => Record<string, string>;
}

export const harnesses: Harness[] = [
  {
    id: "generated",
    cliName: "gemini-api",
    binDir: path.join(evalRoot, "bin", "generated"),
    plumbing: (baseUrl) =>
      `For this session the API is served at ${baseUrl}; pass --server-url ${baseUrl} to every ${"`gemini-api`"} invocation.`,
    // The API version global has no default; real users set it once via env or
    // config. Provide it as environment plumbing, mirroring the reference
    // CLI's SDK-side default of v1beta.
    env: () => ({ GEMINI_API_VERSION: "v1beta" }),
  },
  {
    id: "reference",
    cliName: "gemini-api-cli",
    binDir: path.join(evalRoot, "bin", "reference"),
    plumbing: () =>
      "The CLI is already configured via the environment to talk to the correct API server.",
    env: (baseUrl) => ({ GOOGLE_GEMINI_BASE_URL: baseUrl }),
  },
];

export function systemPrompt(h: Harness, baseUrl: string): string {
  return [
    "You are an autonomous operator of a command-line tool. You can only use the Bash tool.",
    `The \`${h.cliName}\` CLI is installed and on PATH. A valid Gemini API key is already set in the environment.`,
    h.plumbing(baseUrl),
    "Discover the CLI's usage with --help as needed. Never launch interactive or TUI modes; every command must run non-interactively and exit on its own.",
    "Complete the user's task with as few commands as possible, then reply with one line starting with DONE: followed by the answer.",
  ].join("\n");
}
