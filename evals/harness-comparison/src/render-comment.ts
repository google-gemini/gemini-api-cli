// Render the PR comment from a saved results JSON (no agent runs).
// Usage: tsx src/render-comment.ts results/run-....json
import fs from "node:fs";
import { prComment, type RunResult } from "./report.js";

const file = process.argv[2];
if (!file) {
  console.error("usage: tsx src/render-comment.ts <results.json>");
  process.exit(1);
}
const results = JSON.parse(fs.readFileSync(file, "utf8")) as RunResult[];
console.log(prComment(results, undefined));
