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

import { DEFAULT_SERVER_TIMEOUT_SECONDS } from "./api";
import type { ContentBlock, StreamEvent, StreamResult } from "./stream";

export function printCurl(
  method: string,
  url: string,
  apiKey: string,
  body?: unknown,
  headers?: Record<string, string>,
): void {
  const serverTimeout = headers?.["x-server-timeout"] ?? DEFAULT_SERVER_TIMEOUT_SECONDS.toString();
  const apiRevision = url.includes("/interactions")
    ? ` \\\n  -H "Api-Revision: 2026-05-20"`
    : "";

  let curl = `curl -X ${method} "${url}" \\
  -H "Content-Type: application/json" \\
  -H "x-goog-api-key: ${apiKey}" \\
  -H "x-server-timeout: ${serverTimeout}"${apiRevision}`;

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

  // Verbose state accumulation
  private currentStepThought: any = null;
  private currentStepFunctionCall: any = null;
  private currentStepFunctionResult: any = null;
  private currentStepCodeCall: any = null;
  private currentStepCodeResult: any = null;
  private currentStepModelOutput: any = null;

  // Normal mode buffering (to combine tool call + result)
  private bufferedToolCall: string | null = null;
  private bufferedToolType: string | null = null;

  constructor(
    private stdout: typeof process.stdout = process.stdout,
    private verbose = false,
  ) {}

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

  private codeExecutionIsError = false;

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

    if (this.verbose) {
      this.currentStepThought = null;
      this.currentStepFunctionCall = null;
      this.currentStepFunctionResult = null;
      this.currentStepCodeCall = null;
      this.currentStepCodeResult = null;
      this.currentStepModelOutput = null;
      return;
    }

    if (this.currentStepType === "thought") {
      const prefix = this.getPrefix("thought");
      this.stdout.write(`${prefix}\n`);
      this.prefixPrinted = true;
    } else if (this.currentStepType === "model_output") {
      let hasText = false;
      if (Array.isArray(event.data.step?.content)) {
        hasText = event.data.step.content.some((c: any) => c.type === "text" && c.text);
      }
      if (hasText) {
        const prefix = this.getPrefix("text");
        this.stdout.write(`${prefix}\n`);
        this.prefixPrinted = true;
        for (const c of event.data.step.content) {
          if (c.type === "text" && c.text) {
            this.stdout.write(c.text);
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

    if ((!this.currentStepType || this.currentStepType === "unknown") && block?.type) {
      this.currentStepType = block.type;
    }
    const type = this.currentStepType || "text";

    if (this.verbose) {
      if (type === "thought" || type === "thought_summary" || type === "thought_signature") {
        if (!this.currentStepThought) this.currentStepThought = {};
        if (delta.signature) this.currentStepThought.signature = delta.signature;
        if (delta.text)
          this.currentStepThought.text = (this.currentStepThought.text || "") + delta.text;
      } else if (type === "function_call") {
        if (!this.currentStepFunctionCall)
          this.currentStepFunctionCall = { name: "", arguments: "" };
        if (delta.name) this.currentStepFunctionCall.name = delta.name;
        if (delta.id) this.currentStepFunctionCall.id = delta.id;
        if (delta.arguments) {
          if (typeof delta.arguments === "string") {
            this.currentStepFunctionCall.arguments += delta.arguments;
          } else {
            this.currentStepFunctionCall.arguments = delta.arguments;
          }
        }
      } else if (type === "function_result") {
        if (!this.currentStepFunctionResult) this.currentStepFunctionResult = { result: "" };
        if (delta.name) this.currentStepFunctionResult.name = delta.name;
        if (delta.call_id) this.currentStepFunctionResult.call_id = delta.call_id;
        if (delta.result) {
          if (typeof delta.result === "string") {
            this.currentStepFunctionResult.result += delta.result;
          } else {
            this.currentStepFunctionResult.result = delta.result;
          }
        }
      } else if (type === "code_execution_call") {
        if (!this.currentStepCodeCall) this.currentStepCodeCall = { code: "" };
        if (delta.id) this.currentStepCodeCall.id = delta.id;
        if (delta.arguments) {
          if (delta.arguments.language)
            this.currentStepCodeCall.language = delta.arguments.language;
          if (delta.arguments.code) this.currentStepCodeCall.code += delta.arguments.code;
        }
      } else if (type === "code_execution_result") {
        if (!this.currentStepCodeResult)
          this.currentStepCodeResult = { result: "", is_error: false };
        if (delta.call_id) this.currentStepCodeResult.call_id = delta.call_id;
        if (delta.is_error !== undefined) this.currentStepCodeResult.is_error = delta.is_error;
        if (delta.result) this.currentStepCodeResult.result += delta.result;
      } else if (type === "model_output" || type === "text") {
        if (!this.currentStepModelOutput) this.currentStepModelOutput = { content: [] };
        if (delta.text) {
          let textPart = this.currentStepModelOutput.content.find((c: any) => c.type === "text");
          if (!textPart) {
            textPart = { type: "text", text: "" };
            this.currentStepModelOutput.content.push(textPart);
          }
          textPart.text += delta.text;
        }
        if (delta.data) {
          let mediaPart = this.currentStepModelOutput.content.find((c: any) => c.type !== "text");
          if (!mediaPart) {
            mediaPart = { type: delta.type || "image", data: "" };
            this.currentStepModelOutput.content.push(mediaPart);
          }
          mediaPart.data += delta.data;
          if (delta.mime_type) mediaPart.mimeType = delta.mime_type;
        }
      }
      return;
    }

    if (type === "thought" || type === "thought_summary" || type === "thought_signature") {
      return;
    }

    if (type === "function_call" || type === "code_execution_call") {
      if (delta.arguments) {
        // Assume delta.arguments is streamed as string chunks containing JSON fragments.
        // If it is pre-parsed or delivered as objects, concatenation will result in invalid JSON.
        this.accumulatedArguments +=
          typeof delta.arguments === "string" ? delta.arguments : JSON.stringify(delta.arguments);
      }
      if (delta.name) {
        this.currentStepName = delta.name;
      }
      return;
    }

    if (type === "function_result") {
      if (delta.result) {
        this.accumulatedResult +=
          typeof delta.result === "string" ? delta.result : JSON.stringify(delta.result);
      }
      if (delta.name) {
        this.currentStepName = delta.name;
      }
      if (delta.call_id) {
        // Just in case
      }
      return;
    }

    if (type === "code_execution_result") {
      if (delta.result) this.accumulatedResult += delta.result;
      if (delta.is_error !== undefined) this.codeExecutionIsError = delta.is_error;
      return;
    }

    if ((type === "model_output" || type === "text") && delta.text) {
      if (!this.prefixPrinted) {
        const prefix = this.getPrefix("text");
        this.stdout.write(`${prefix}\n`);
        this.prefixPrinted = true;
      }
      this.stdout.write(delta.text);
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

    if (this.verbose) {
      const stepObj: any = {
        index: this.currentStepIndex,
        type: type || "unknown",
        status: "completed",
      };

      if (type === "thought" && this.currentStepThought) {
        stepObj.thought = this.currentStepThought;
      } else if (type === "function_call") {
        let args = this.currentStepFunctionCall?.arguments || this.accumulatedArguments;
        if (typeof args === "string" && args.trim()) {
          try {
            args = JSON.parse(args);
          } catch {}
        }
        stepObj.function_call = {
          name: this.currentStepFunctionCall?.name || name || "unknown",
          arguments: args,
          id: this.currentStepFunctionCall?.id,
        };
      } else if (type === "function_result") {
        let res = this.currentStepFunctionResult?.result || this.accumulatedResult;
        if (typeof res === "string" && res.trim()) {
          try {
            res = JSON.parse(res);
          } catch {}
        }
        stepObj.function_result = {
          name: this.currentStepFunctionResult?.name || name,
          result: res,
          call_id: this.currentStepFunctionResult?.call_id,
        };
      } else if (type === "code_execution_call") {
        let code = this.currentStepCodeCall?.code || "";
        if (!code && this.accumulatedArguments) {
          try {
            const parsed = JSON.parse(this.accumulatedArguments);
            code = parsed.code || parsed.raw || "";
          } catch {
            code = this.accumulatedArguments;
          }
        }
        stepObj.code_execution_call = {
          language: this.currentStepCodeCall?.language || "python",
          code,
          id: this.currentStepCodeCall?.id,
        };
      } else if (type === "code_execution_result") {
        stepObj.code_execution_result = {
          result: this.currentStepCodeResult?.result || this.accumulatedResult,
          is_error: this.currentStepCodeResult?.is_error || false,
          call_id: this.currentStepCodeResult?.call_id,
        };
      } else if ((type === "model_output" || type === "text") && this.currentStepModelOutput) {
        stepObj.model_output = this.currentStepModelOutput;
      }

      this.stdout.write(JSON.stringify(stepObj) + "\n");

      // Reset verbose states
      this.currentStepThought = null;
      this.currentStepFunctionCall = null;
      this.currentStepFunctionResult = null;
      this.currentStepCodeCall = null;
      this.currentStepCodeResult = null;
      this.currentStepModelOutput = null;

      this.currentStepIndex = null;
      this.currentStepType = null;
      this.currentStepName = null;
      this.accumulatedArguments = "";
      this.accumulatedResult = "";
      return;
    }

    if (type === "function_call") {
      let argsObj: any = {};
      try {
        argsObj = JSON.parse(this.accumulatedArguments);
      } catch {
        argsObj = { raw: this.accumulatedArguments };
      }

      if (name === "write_file") {
        const path = argsObj.path || "unknown";
        this.bufferedToolCall = `write_file(path="${path}")`;
      } else {
        const argsStr = JSON.stringify(argsObj);
        const truncatedArgs = argsStr.length > 100 ? argsStr.substring(0, 100) + "..." : argsStr;
        this.bufferedToolCall = `${name || "unknown"}(${truncatedArgs})`;
      }
      this.bufferedToolType = "function_call";
    } else if (type === "code_execution_call") {
      let argsObj: any = {};
      try {
        argsObj = JSON.parse(this.accumulatedArguments);
      } catch {
        argsObj = { code: this.accumulatedArguments };
      }
      const code = argsObj.code || argsObj.raw || "";
      const cleanCode = code.trim().replace(/\n/g, "; ");
      this.bufferedToolCall = cleanCode;
      this.bufferedToolType = "code_execution_call";
    } else if (type === "function_result") {
      const prefix = this.getPrefix("function_call");
      let resultObj: any = {};
      try {
        resultObj = JSON.parse(this.accumulatedResult);
      } catch {
        resultObj = this.accumulatedResult;
      }

      let resultStr = "";
      if (resultObj && typeof resultObj === "object") {
        if (resultObj.error) {
          resultStr = `Error: ${resultObj.error}`;
        } else {
          resultStr = JSON.stringify(resultObj);
        }
      } else {
        resultStr = String(resultObj);
      }

      this.stdout.write(`${prefix} ${this.bufferedToolCall || "unknown()"} -> ${resultStr}\n`);
      this.bufferedToolCall = null;
      this.bufferedToolType = null;
    } else if (type === "code_execution_result") {
      const prefix = this.getPrefix("code_execution_call");
      const resultStr = this.accumulatedResult.trim();

      let displayResult = "";
      if (this.codeExecutionIsError) {
        displayResult = `Error: ${resultStr}`;
      } else {
        displayResult = resultStr ? `"${resultStr.replace(/\n/g, "\\n")}"` : "success";
      }

      this.stdout.write(`${prefix} ${this.bufferedToolCall || "code"} -> ${displayResult}\n`);
      this.bufferedToolCall = null;
      this.bufferedToolType = null;
      this.codeExecutionIsError = false;
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

export function printCompletionSummary(
  result: StreamResult,
  latencySeconds: number,
  verbose = false,
): void {
  if (verbose) {
    const summaryObj: any = {
      interaction: {
        id: result.interactionId,
        status: result.status || "completed",
      },
    };
    if (result.environmentId) {
      summaryObj.interaction.environment_id = result.environmentId;
    }
    if (result.usage) {
      const inTokens = result.usage.inputTokens || 0;
      const outTokens = result.usage.outputTokens || 0;
      summaryObj.interaction.usage = {
        total_tokens: inTokens + outTokens,
        total_input_tokens: inTokens,
        total_output_tokens: outTokens,
        total_thought_tokens: result.usage.thoughtTokens || 0,
        total_cached_tokens: result.usage.cachedTokens || 0,
      };
    }
    if (result.created) summaryObj.interaction.created = result.created;
    if (result.updated) summaryObj.interaction.updated = result.updated;
    summaryObj.interaction.object = "interaction";

    console.log(JSON.stringify(summaryObj));
  } else {
    console.log("\n✓ completed");
    console.log(`  interaction_id: ${result.interactionId}`);

    if (result.environmentId) {
      console.log(`  environment_id: ${result.environmentId}`);
    }

    if (result.usage) {
      const inTokens = result.usage.inputTokens?.toLocaleString() ?? "0";
      const outTokens = result.usage.outputTokens?.toLocaleString() ?? "0";
      const thoughtTokens = result.usage.thoughtTokens?.toLocaleString() ?? "0";
      const cachedTokens = result.usage.cachedTokens !== undefined ? ` cached:${result.usage.cachedTokens.toLocaleString()}` : "";
      console.log(`  tokens: in:${inTokens} out:${outTokens} thought:${thoughtTokens}${cachedTokens}`);
    }
    console.log(`  latency: ${latencySeconds.toFixed(1)}s`);
  }
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
