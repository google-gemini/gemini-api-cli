// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import type { ContentBlock, StreamEvent, StreamResult } from "./stream";

export function printCurl(method: string, url: string, apiKey: string, body?: unknown): void {
  let curl = `curl -X ${method} "${url}" \\\n`;
  curl += `  -H "Content-Type: application/json" \\\n`;
  curl += `  -H "x-goog-api-key: ${apiKey}" \\\n`;
  curl += `  -H "x-server-timeout: 600"`;

  if (url.includes("/interactions")) {
    curl += ` \\\n  -H "Api-Revision: 2026-05-20"`;
  }

  if (body) {
    // Escape single quotes in body for bash
    const bodyStr = JSON.stringify(body, null, 2).replace(/'/g, "'\\''");
    curl += ` \\\n  -d '${bodyStr}'`;
  }

  console.log(curl);
}

export class HumanStreamRenderer {
  private currentBlockIndex: number | null = null;
  private prefixPrinted = false;
  private colWidth = 15;
  private wasFunctionCall = false;

  constructor(private stdout: typeof process.stdout = process.stdout) {}

  private getPrefix(type: string): string {
    const prefixes: Record<string, string> = {
      text: "[text]",
      thought_summary: "[thought]",
      function_call: "[tool]",
      function_result: "[result]",
      code_execution_call: "[code]",
      code_execution_result: "[result]",
      google_search_call: "[search]",
      google_search_result: "[grounding]",
      url_context_call: "[url]",
      url_context_result: "[url-result]",
      mcp_server_tool_call: "[mcp]",
      mcp_server_tool_result: "[mcp-result]",
      file_search_call: "[file-search]",
      file_search_result: "[file-result]",
      google_maps_call: "[maps]",
      google_maps_result: "[maps-result]",
      image: "[image]",
      audio: "[audio]",
      video: "[video]",
      document: "[document]",
    };
    return type in prefixes ? prefixes[type] : `[${type}]`;
  }

  handleStepEvent(event: StreamEvent) {
    if (event.type === "step.start") {
      const stepType = event.data.step?.type || "unknown";
      this.stdout.write(`${"[step]".padEnd(this.colWidth)}▸ ${stepType} started\n`);

      // Print content if available in step.start (e.g. for short responses)
      if (event.data.step?.type === "model_output" && Array.isArray(event.data.step.content)) {
        for (const c of event.data.step.content) {
          if (c.type === "text" && c.text) {
            this.stdout.write(`${"[text]".padEnd(this.colWidth)}${c.text}\n`);
          }
        }
      }

      if (event.data.step?.type === "code_execution_result" && event.data.step.result) {
        this.stdout.write(`${"[result]".padEnd(this.colWidth)}${event.data.step.result}\n`);
      }
    } else if (event.type === "step.stop") {
      const stepType = event.data.step?.type || "unknown";
      const status = event.data.step?.status || "completed";
      this.stdout.write(`${"[step]".padEnd(this.colWidth)}▪ ${stepType} ${status}\n`);
    }
  }

  handleEvent(event: StreamEvent, type: string, block?: ContentBlock) {
    if (event.type !== "content.delta" && event.type !== "step.delta") return;
    if (type === "thought" || type === "thought_summary" || type === "thought_signature") return;

    const index = event.data.index ?? event.data.step_index;
    const delta = event.data.delta;

    if (this.currentBlockIndex !== index) {
      if (this.currentBlockIndex !== null) {
        if (this.wasFunctionCall) {
          this.stdout.write(")");
        }
        this.stdout.write("\n");
      }
      this.currentBlockIndex = index;
      this.prefixPrinted = false;
      this.wasFunctionCall = false;
    }

    const prefix = this.getPrefix(type);
    let content = "";

    if (delta.text) content = delta.text;
    else if (delta.arguments)
      content =
        typeof delta.arguments === "string" ? delta.arguments : JSON.stringify(delta.arguments);
    else if (delta.code) content = delta.code;
    else if (delta.result)
      content = typeof delta.result === "string" ? delta.result : JSON.stringify(delta.result);
    else if (delta.query) content = delta.query;
    else if (delta.url) content = delta.url;
    else if (delta.name) content = delta.name;

    if (content) {
      if (prefix) {
        if (!this.prefixPrinted) {
          this.stdout.write(prefix.padEnd(this.colWidth));

          if (block && (block as any).name) {
            this.stdout.write(`${(block as any).name}(`);
            this.wasFunctionCall = true;
          }

          this.prefixPrinted = true;
        }

        if (content.includes("\n")) {
          const lines = content.split("\n");
          this.stdout.write(lines[0]);
          for (let i = 1; i < lines.length; i++) {
            this.stdout.write(`\n${"".padEnd(this.colWidth)}${lines[i]}`);
          }
        } else {
          this.stdout.write(content);
        }
      } else {
        this.stdout.write(content);
      }
    }
  }

  finish() {
    if (this.wasFunctionCall) {
      this.stdout.write(")");
    }
    this.stdout.write("\n");
  }
}

export function printCompletionSummary(result: StreamResult, latencySeconds: number): void {
  console.log("\n✓ completed");
  console.log(`  interaction_id: ${result.interactionId}`);

  if (result.environmentId) {
    console.log(`  environment_id: ${result.environmentId}`);
  }

  if (result.usage) {
    const inTokens = result.usage.inputTokens?.toLocaleString() ?? "0";
    const outTokens = result.usage.outputTokens?.toLocaleString() ?? "0";
    const thoughtTokens = result.usage.thoughtTokens?.toLocaleString() ?? "0";
    console.log(`  tokens: in:${inTokens} out:${outTokens} thought:${thoughtTokens}`);
  }
  console.log(`  latency: ${latencySeconds.toFixed(1)}s`);
}

export function printError(message: string, tryCommands?: string[]): void {
  console.error(`✗ ${message}\n`);
  if (tryCommands && tryCommands.length > 0) {
    console.error("  Try:");
    for (const cmd of tryCommands) {
      console.error(`    ${cmd}`);
    }
  }
}

export function printBlock(block: ContentBlock): void {
  if (block.type === "text") return;

  const prefixes: Record<string, string> = {
    function_call: "[tool]",
    function_result: "[result]",
    code_execution_call: "[code]",
    code_execution_result: "[result]",
    google_search_call: "[search]",
    google_search_result: "[grounding]",
    url_context_call: "[url]",
    url_context_result: "[url-result]",
    mcp_server_tool_call: "[mcp]",
    mcp_server_tool_result: "[mcp-result]",
    file_search_call: "[file-search]",
    file_search_result: "[file-result]",
    google_maps_call: "[maps]",
    google_maps_result: "[maps-result]",
    image: "[image]",
    audio: "[audio]",
    video: "[video]",
    document: "[document]",
  };

  const prefix = prefixes[block.type] || `[${block.type}]`;
  const colWidth = 15;
  const prefixStr = prefix.padEnd(colWidth);

  switch (block.type) {
    case "function_call":
      console.log(`${prefixStr}${block.name}(${JSON.stringify(block.arguments)})`);
      break;
    case "function_result":
      console.log(
        `${prefixStr}${typeof block.result === "string" ? block.result : JSON.stringify(block.result)}`,
      );
      break;
    case "code_execution_call":
      console.log(`${prefixStr}\`\`\`\n${block.arguments?.code || ""}\n\`\`\``);
      break;
    case "code_execution_result":
      console.log(`${prefixStr}${block.result}`);
      break;
    case "google_search_call":
      console.log(`${prefixStr}query: ${block.query}`);
      break;
    default: {
      const blockStr = JSON.stringify(block);
      if (blockStr.length < 100) {
        console.log(`${prefixStr}${blockStr}`);
      } else {
        console.log(`${prefixStr}${block.type} completed`);
      }
      break;
    }
  }
}

export function printPollingStatus(elapsedSeconds: number, status: string = "in_progress"): void {
  console.error(`\r⟳ deep-research ${status}... (${Math.round(elapsedSeconds)}s elapsed)`);
}
