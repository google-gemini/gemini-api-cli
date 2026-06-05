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

// gemini-api run command

import { readFileSync } from "node:fs";
import { defineCommand } from "citty";
import {
  apiGetRequest,
  apiRequest,
  apiStreamRequest,
  buildInteractionRequest,
  isDeepResearchAgent,
  parseSourceFlag,
  parseToolFlag,
  type RunOptions,
  resolveContext,
  type Source,
  type Tool,
} from "../lib/api";
import { inputToContentBlock, saveMediaOutputs } from "../lib/files";
import { logRequest, logResponse } from "../lib/logger";
import {
  HumanStreamRenderer,
  mapContentToStepEvent,
  printCompletionSummary,
  printCurl,
  printError,
  printPollingStatus,
  renderStepEvent,
} from "../lib/output";
import { globalFlags } from "../lib/shared-args";
import type { StreamResult } from "../lib/stream";
import { processStream } from "../lib/stream";

const DEEP_RESEARCH_POLL_INTERVAL_MS = 10_000;

export default defineCommand({
  meta: {
    name: "run",
    description: `Create an interaction against a model or agent.

Examples:
  gemini-api run "What is the capital of France?"
  gemini-api run "Explain quantum computing" --model gemini-3.1-pro-preview`,
  },
  args: {
    ...globalFlags,
    prompt: {
      type: "positional",
      description: "Input prompt. Use '-' for stdin.",
      required: false,
    },
    model: {
      type: "string",
      alias: "m",
      description: "Model to use",
      default: "gemini-3.5-flash",
    },
    agent: {
      type: "string",
      alias: "a",
      description: "Agent to use (overrides --model)",
    },
    "previous-interaction-id": {
      type: "string",
      alias: "p",
      description: "Continue from previous interaction",
    },
    "system-instruction": {
      type: "string",
      alias: "s",
      description: "System instruction",
    },
    "service-tier": {
      type: "string",
      description: "Service tier (flex, standard, priority)",
    },
    environment: {
      type: "string",
      description: "Environment to use ('remote' or a specific env_id)",
    },
    network: {
      type: "string",
      description: "Disable network access ('disabled')",
    },
    "network-allowlist": {
      type: "string",
      description: "Comma-separated domain allowlist for network egress",
    },
    input: {
      type: "string",
      alias: "i",
      description: "Additional input (can be specified multiple times): <type>:<path_or_url>",
    },
    tool: {
      type: "string",
      description:
        "Tool declaration (can be specified multiple times): code_execution, google_search, mcp_server:name:url, function:name:schema",
    },
    source: {
      type: "string",
      description:
        "Environment source (can be specified multiple times): inline:<target>:<content>, github:<url>:<target>, gcs:<source>:<target>",
    },
    "tool-choice": {
      type: "string",
      description: "Tool choice mode (auto, any, none, validated)",
    },
    "response-modality": {
      type: "string",
      description: "Requested output types (e.g., image, audio)",
    },
    "response-mime-type": {
      type: "string",
      description: "MIME type for response (e.g., application/json)",
    },
    output: {
      type: "string",
      alias: "o",
      description: "Save generated media to file",
    },
    "aspect-ratio": {
      type: "string",
      description: "Image aspect ratio (1:1, 16:9, etc.)",
    },
    "image-size": {
      type: "string",
      description: "Image size (512, 1K, 2K, 4K)",
    },
    "edit-strength": {
      type: "string",
      description: "How much to change the original image (0.0 to 1.0)",
    },
    mask: {
      type: "string",
      description: "Path to a mask image for localized editing",
    },
    voice: {
      type: "string",
      description: "TTS voice name",
    },
    language: {
      type: "string",
      description: "Language code for TTS",
    },
  },
  async run({ args }) {
    let prompt = args.prompt;
    if (!prompt && process.argv[process.argv.length - 1] === "-") {
      prompt = "-";
    }
    if (prompt === "-") {
      // Read from stdin
      prompt = await new Promise<string>((resolve) => {
        let data = "";
        process.stdin.on("data", (chunk) => {
          data += chunk;
        });
        process.stdin.on("end", () => {
          resolve(data);
        });
      });
      if (!prompt.trim()) {
        console.error("✗ Stdin was empty.");
        process.exit(1);
      }
    }

    if (!prompt) {
      printError("Missing prompt.", [
        'gemini-api run "Your prompt here"',
        'echo "Your prompt" | gemini-api run -',
      ]);
      process.exit(1);
    }

    const sharedFlags = {
      apiKey: (args["api-key"] || args.apiKey) as string | undefined,
      baseUrl: (args["base-url"] || args.baseUrl) as string | undefined,
      json: args.json as boolean,
      dryRun: (args["dry-run"] || args.dryRun) as boolean,
    };

    const ctx = resolveContext(sharedFlags);

    // Parse repeated --input flags from process.argv
    // (citty doesn't natively support repeated flags well)
    const inputs: string[] = [];
    for (let i = 0; i < process.argv.length; i++) {
      if (process.argv[i] === "--input" || process.argv[i] === "-i") {
        if (i + 1 < process.argv.length) {
          inputs.push(process.argv[i + 1]);
          i++; // Skip the value
        }
      }
    }

    // Parse repeated --tool flags from process.argv
    const toolStrings: string[] = [];
    for (let i = 0; i < process.argv.length; i++) {
      if (process.argv[i] === "--tool") {
        if (i + 1 < process.argv.length) {
          toolStrings.push(process.argv[i + 1]);
          i++;
        }
      }
    }

    // Parse tools
    let tools: Tool[] | undefined;
    if (toolStrings.length > 0) {
      tools = [];
      for (const toolStr of toolStrings) {
        tools.push(parseToolFlag(toolStr));
      }
    }

    // Parse repeated --source flags from process.argv
    const sourceStrings: string[] = [];
    for (let i = 0; i < process.argv.length; i++) {
      if (process.argv[i] === "--source") {
        if (i + 1 < process.argv.length) {
          sourceStrings.push(process.argv[i + 1]);
          i++;
        }
      }
    }

    // Parse sources
    let sources: Source[] | undefined;
    if (sourceStrings.length > 0) {
      sources = [];
      for (const sourceStr of sourceStrings) {
        sources.push(parseSourceFlag(sourceStr));
      }
    }

    let interactionInput: any = prompt;

    if (inputs.length > 0) {
      const parts: any[] = [{ type: "text", text: prompt }];
      for (const inputStr of inputs) {
        try {
          const block = inputToContentBlock(inputStr);
          parts.push(block);
        } catch (error) {
          printError((error as Error).message);
          process.exit(1);
        }
      }
      interactionInput = parts;
    }

    let maskData: string | undefined;
    if (args.mask) {
      try {
        const data = readFileSync(args.mask as string);
        maskData = data.toString("base64");
      } catch (error: any) {
        if (error.code === "ENOENT") {
          printError(`File not found: ${args.mask}`);
          process.exit(1);
        }
        throw error;
      }
    }

    let environment: any;
    if (args.environment) {
      environment = args.environment;
    }

    if (args.network || args["network-allowlist"]) {
      const envObj: any = { type: "remote" };
      if (args.network === "disabled") {
        envObj.network = "disabled";
      } else if (args["network-allowlist"]) {
        const domains = (args["network-allowlist"] as string).split(",").map((d) => d.trim());
        envObj.network = {
          allowlist: domains.map((domain) => ({ domain })),
        };
      }
      environment = envObj;
    }

    const runOpts: RunOptions = {
      model: args.agent ? undefined : (args.model as string | undefined),
      agent: args.agent as string | undefined,
      input: interactionInput,
      systemInstruction: args["system-instruction"] as string | undefined,
      tools,
      sources,
      serviceTier: args["service-tier"] as string | undefined,
      previousInteractionId: args["previous-interaction-id"] as string | undefined,
      stream: !isDeepResearchAgent(args.agent as string | undefined),
      toolChoice: args["tool-choice"] as string | undefined,

      responseModalities: args["response-modality"]
        ? [args["response-modality"] as string]
        : undefined,
      responseMimeType: args["response-mime-type"] as string | undefined,
      voice: args.voice as string | undefined,
      language: args.language as string | undefined,
      aspectRatio: args["aspect-ratio"] as string | undefined,
      imageSize: args["image-size"] as string | undefined,
      editStrength: args["edit-strength"] ? parseFloat(args["edit-strength"] as string) : undefined,
      mask: maskData,
      environment,
    };

    const body = buildInteractionRequest(runOpts);

    if (args["dry-run"]) {
      printCurl("POST", `${ctx.baseUrl}/interactions`, ctx.apiKey, body);
      return;
    }

    const startTime = performance.now();

    // Deep Research agents use streaming with auto-reconnect
    if (isDeepResearchAgent(args.agent as string | undefined)) {
      await runDeepResearch(ctx, body, args, startTime);
      return;
    }

    // Standard model/agent streaming
    const response = await apiStreamRequest(ctx, "/interactions", body);

    if (args.json) {
      await processStream(response, {
        onEvent: (event) => {
          const data = event.data;
          if (data.event_type === "content.delta" && data.delta?.type === "thought_signature") {
            return;
          }
          console.log(event.raw);
        },
        onComplete: () => {},
      });
    } else {
      const verbose = args.verbose as boolean;
      const renderer = new HumanStreamRenderer(process.stdout, verbose);

      await processStream(response, {
        onEvent: (event, block) => {
          const mapped = mapContentToStepEvent(event);
          renderStepEvent(renderer, mapped, block);
        },
        onComplete: (result) => {
          renderer.finish();
          saveMediaOutputs(result.outputs, result.interactionId, args.output as string | undefined);
          const latencySeconds = (performance.now() - startTime) / 1000;
          printCompletionSummary(result, latencySeconds, verbose);
          if (!args.json) {
            logRequest(result.interactionId, body);
            logResponse(result.interactionId, result);
          }
        },
      });
    }
  },
});

/**
 * Run a Deep Research agent with streaming and automatic reconnection.
 *
 * Deep Research tasks can take minutes. The SSE connection may drop (e.g., after
 * the 600s server timeout). This function handles:
 * 1. Initial POST to start the task (stream=true, background=true)
 * 2. Processing stream events as they arrive
 * 3. If the connection drops while status is still in_progress, poll the
 *    interaction status and reconnect the stream using last_event_id
 */
async function runDeepResearch(
  ctx: import("../lib/api").CLIContext,
  body: object,
  args: any,
  startTime: number,
): Promise<void> {
  let interactionId = "";
  let isComplete = false;
  const result: StreamResult = {
    status: "in_progress",
    outputs: [],
    steps: [],
    interactionId: "",
  };

  console.error(`⟳ Starting deep research...`);
  try {
    const response = await apiRequest<any>(ctx, "POST", "/interactions", body);
    interactionId = response.id || response.interaction_id;
    result.interactionId = interactionId;
    console.error(`✓ Deep research started. Interaction ID: ${interactionId}`);
  } catch (error) {
    printError(`Failed to start deep research: ${(error as Error).message}`);
    process.exit(1);
  }

  let retryCount = 0;
  const maxRetries = 5;

  // 2. Polling loop
  while (!isComplete && interactionId) {
    const elapsedSeconds = (performance.now() - startTime) / 1000;
    printPollingStatus(elapsedSeconds);

    // Wait before polling
    await new Promise((resolve) => setTimeout(resolve, DEEP_RESEARCH_POLL_INTERVAL_MS));

    // Check interaction status via GET
    try {
      const status = await apiGetRequest<any>(ctx, `/interactions/${interactionId}`);
      retryCount = 0; // Reset retry count on success

      if (status.status === "completed" || status.status === "failed") {
        isComplete = true;
        result.status = status.status;
        result.interactionId = interactionId;

        if (status.usage) {
          result.usage = {
            inputTokens: status.usage.total_input_tokens ?? status.usage.input_tokens,
            outputTokens: status.usage.total_output_tokens ?? status.usage.output_tokens,
            thoughtTokens: status.usage.total_thought_tokens ?? status.usage.thought_tokens,
          };
        }

        // Print final outputs
        if (status.outputs && status.outputs.length > 0) {
          for (const output of status.outputs) {
            if (output.type === "text" && output.text) {
              process.stdout.write(output.text);
            }
          }
        }
        break;
      }

      if (status.status !== "in_progress") {
        // Unexpected status
        isComplete = true;
        break;
      }
    } catch (_error) {
      retryCount++;
      const elapsedSeconds = (performance.now() - startTime) / 1000;
      console.error(
        `⟳ Polling failed, retrying (${retryCount}/${maxRetries})... (${Math.round(elapsedSeconds)}s elapsed)`,
      );
      if (retryCount >= maxRetries) {
        printError(`Failed to poll status after ${maxRetries} attempts.`);
        process.exit(1);
      }
      // Wait longer on failure
      await new Promise((resolve) => setTimeout(resolve, DEEP_RESEARCH_POLL_INTERVAL_MS * 2));
    }
  }

  // 3. Finalize output
  saveMediaOutputs(result.outputs, result.interactionId, args.output as string | undefined);
  const latencySeconds = (performance.now() - startTime) / 1000;
  if (!args.json) {
    printCompletionSummary(result, latencySeconds);
    logRequest(result.interactionId, body);
    logResponse(result.interactionId, result);
  }
}
