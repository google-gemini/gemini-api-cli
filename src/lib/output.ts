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
  private currentStepIndex: number | null = null;
  private currentStepType: string | null = null;
  private currentStepName: string | null = null;
  private prefixPrinted = false;
  private accumulatedArguments = "";
  private accumulatedResult = "";
  private colWidth = 15;

  constructor(private stdout: typeof process.stdout = process.stdout) {}

  private getPrefix(type: string): string {
    const prefixes: Record<string, string> = {
      text: "[text]",
      thought: "[thought]",
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

  handleStepStart(event: StreamEvent, block?: ContentBlock) {
    const index = event.data.index ?? event.data.step_index;
    if (index === undefined) return;

    // Finalize previous step if index changed
    if (this.currentStepIndex !== null && this.currentStepIndex !== index) {
      this.finalizeStep();
    }

    this.currentStepIndex = index;
    this.currentStepType = event.data.step?.type || "unknown";
    this.currentStepName = event.data.step?.name || null;
    this.prefixPrinted = false;
    this.accumulatedArguments = "";
    this.accumulatedResult = "";

    if (this.currentStepType === "thought") {
      const prefix = this.getPrefix("thought").padEnd(this.colWidth);
      this.stdout.write(`${prefix}▸ thinking\n`);
      this.prefixPrinted = true;
    } else if (this.currentStepType === "model_output") {
      if (Array.isArray(event.data.step?.content)) {
        for (const c of event.data.step.content) {
          if (c.type === "text" && c.text) {
            const prefix = this.getPrefix("text").padEnd(this.colWidth);
            this.stdout.write(`${prefix}${c.text}`);
            this.prefixPrinted = true;
          }
        }
      }
    }
  }

  handleStepDelta(event: StreamEvent, block?: ContentBlock) {
    const index = event.data.index ?? event.data.step_index;
    if (index === undefined || this.currentStepIndex !== index) return;

    const delta = event.data.delta;
    if (!delta) return;

    const type = this.currentStepType || "text";

    if (type === "thought" || type === "thought_summary" || type === "thought_signature") {
      return;
    }

    if (type === "function_call" || type === "code_execution_call") {
      if (delta.arguments) {
        this.accumulatedArguments += typeof delta.arguments === "string"
          ? delta.arguments
          : JSON.stringify(delta.arguments);
      }
      if (delta.name) {
        this.currentStepName = delta.name;
      }
      return;
    }

    if (type === "function_result") {
      if (delta.result) {
        this.accumulatedResult += typeof delta.result === "string"
          ? delta.result
          : JSON.stringify(delta.result);
      }
      if (delta.name) {
        this.currentStepName = delta.name;
      }
      return;
    }

    let content = "";
    let prefixType = type;

    if (type === "model_output") {
      prefixType = "text";
      if (delta.text) content = delta.text;
    } else if (type === "code_execution_result") {
      if (delta.result) content = delta.result;
    } else {
      if (delta.text) content = delta.text;
      else if (delta.result) content = typeof delta.result === "string" ? delta.result : JSON.stringify(delta.result);
    }

    if (content) {
      const prefix = this.getPrefix(prefixType);
      if (prefix) {
        if (!this.prefixPrinted) {
          this.stdout.write(prefix.padEnd(this.colWidth));
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

  handleStepStop(event: StreamEvent, block?: ContentBlock) {
    const index = event.data.index ?? event.data.step_index;
    if (index === undefined || this.currentStepIndex !== index) return;

    this.finalizeStep();
  }

  private finalizeStep() {
    if (this.currentStepIndex === null) return;

    const type = this.currentStepType;
    const name = this.currentStepName;

    if (type === "thought") {
      const prefix = this.getPrefix("thought").padEnd(this.colWidth);
      this.stdout.write(`${prefix}▪ completed\n`);
    } else if (type === "function_call") {
      const prefix = this.getPrefix("function_call").padEnd(this.colWidth);
      let argsObj: any = {};
      try {
        argsObj = JSON.parse(this.accumulatedArguments);
      } catch {
        argsObj = { raw: this.accumulatedArguments };
      }

      if (name === "write_file") {
        const path = argsObj.path || "unknown";
        this.stdout.write(`${prefix}${name}(path="${path}", content=<...>)` + "\n");
      } else {
        const argsStr = JSON.stringify(argsObj);
        const truncatedArgs = argsStr.length > 100 ? argsStr.substring(0, 100) + "..." : argsStr;
        this.stdout.write(`${prefix}${name || "unknown"}(${truncatedArgs})` + "\n");
      }
    } else if (type === "function_result") {
      const prefix = this.getPrefix("function_result").padEnd(this.colWidth);
      let resultObj: any = {};
      try {
        resultObj = JSON.parse(this.accumulatedResult);
      } catch {
        resultObj = this.accumulatedResult;
      }

      const resultStr = typeof resultObj === "string" ? resultObj : JSON.stringify(resultObj);
      const truncatedResult = resultStr.length > 100 ? resultStr.substring(0, 100) + "..." : resultStr;
      this.stdout.write(`${prefix}${name || "tool"}: ${truncatedResult}\n`);
    } else if (type === "code_execution_call") {
      const prefix = this.getPrefix("code_execution_call").padEnd(this.colWidth);
      let argsObj: any = {};
      try {
        argsObj = JSON.parse(this.accumulatedArguments);
      } catch {
        argsObj = { code: this.accumulatedArguments };
      }
      const code = argsObj.code || argsObj.raw || "";
      this.stdout.write(`${prefix}${code}\n`);
    } else if (type === "model_output" || type === "code_execution_result") {
      if (this.prefixPrinted) {
        this.stdout.write("\n");
      }
    }

    this.currentStepIndex = null;
    this.currentStepType = null;
    this.currentStepName = null;
    this.prefixPrinted = false;
    this.accumulatedArguments = "";
    this.accumulatedResult = "";
  }

  finish() {
    this.finalizeStep();
  }
}

/**
 * Maps content.* SSE events to step.* events for the renderer.
 * Some API endpoints still emit content.start/delta/stop; this function
 * normalises them so the renderer only needs to handle step.* events.
 */
export function mapContentToStepEvent(event: StreamEvent): StreamEvent {
  if (event.type === "content.start") {
    return {
      type: "step.start",
      data: {
        index: event.data.index,
        step: { type: event.data.content?.type || "text", status: "in_progress" },
      },
      raw: event.raw,
    };
  }
  if (event.type === "content.delta") {
    return {
      type: "step.delta",
      data: {
        index: event.data.index,
        delta: event.data.delta,
      },
      raw: event.raw,
    };
  }
  if (event.type === "content.stop") {
    return {
      type: "step.stop",
      data: {
        index: event.data.index,
      },
      raw: event.raw,
    };
  }
  return event;
}

/** Dispatch a (possibly mapped) step event to the renderer. */
export function renderStepEvent(
  renderer: HumanStreamRenderer,
  event: StreamEvent,
  block?: ContentBlock,
): void {
  if (event.type === "step.start") {
    renderer.handleStepStart(event, block);
  } else if (event.type === "step.delta") {
    renderer.handleStepDelta(event, block);
  } else if (event.type === "step.stop") {
    renderer.handleStepStop(event, block);
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
