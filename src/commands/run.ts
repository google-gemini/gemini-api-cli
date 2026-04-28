// gemini-api run command
// TODO: Implement — see tasks/task_5.md

import { readFileSync } from "node:fs";
import { defineCommand } from "citty";
import { globalFlags } from "../lib/shared-args";
import { resolveContext, buildInteractionRequest, apiStreamRequest, apiRequest, type RunOptions, type Tool, parseToolFlag } from "../lib/api";
import { processStream } from "../lib/stream";
import { printCurl, HumanStreamRenderer, printCompletionSummary, printError, printBlock } from "../lib/output";
import { inputToContentBlock, saveMediaOutputs } from "../lib/files";
import { CLIError } from "../lib/errors";
import { logRequest, logResponse } from "../lib/logger";

export default defineCommand({
  meta: {
    name: "run",
    description: `Create an interaction against a model or agent.

Examples:
  gemini-api run "What is the capital of France?"
  gemini-api run "Explain quantum computing" --model gemini-3-pro-preview`,
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
      default: "gemini-3-flash-preview",
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
    input: {
      type: "string",
      alias: "i",
      description: "Additional input (can be specified multiple times): <type>:<path_or_url>",
    },
    "response-modality": {
      type: "string",
      description: "Requested output types (e.g., image, audio)",
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
        "gemini-api run \"Your prompt here\"",
        "echo \"Your prompt\" | gemini-api run -"
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

    const inputs: string[] = [];
    for (let i = 0; i < process.argv.length; i++) {
      if (process.argv[i] === "--input" || process.argv[i] === "-i") {
        if (i + 1 < process.argv.length) {
          inputs.push(process.argv[i + 1]);
          i++; // Skip the value
        }
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

    let maskData: string | undefined = undefined;
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

    const runOpts: RunOptions = {
      model: args.agent ? undefined : (args.model as string | undefined),
      agent: args.agent as string | undefined,
      input: interactionInput,
      systemInstruction: args["system-instruction"] as string | undefined,
      serviceTier: args["service-tier"] as string | undefined,
      previousInteractionId: args["previous-interaction-id"] as string | undefined,
      stream: true,

      responseModalities: args["response-modality"] ? [args["response-modality"] as string] : undefined,
      voice: args.voice as string | undefined,
      language: args.language as string | undefined,
      aspectRatio: args["aspect-ratio"] as string | undefined,
      imageSize: args["image-size"] as string | undefined,
      editStrength: args["edit-strength"] ? parseFloat(args["edit-strength"] as string) : undefined,
      mask: maskData,
    };

    const body = buildInteractionRequest(runOpts);

    if (args["dry-run"]) {
      printCurl("POST", `${ctx.baseUrl}/interactions`, ctx.apiKey, body);
      return;
    }

    const startTime = performance.now();
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
      const renderer = new HumanStreamRenderer();
      const activeBlocks = new Map<number, string>();
      
      await processStream(response, {
        onEvent: (event) => {
          if (event.type === "content.start") {
            activeBlocks.set(event.data.index, event.data.content.type);
          } else if (event.type === "content.delta") {
            const type = activeBlocks.get(event.data.index) || "text";
            renderer.handleEvent(event, type);
          }
        },
        onBlockComplete: (block) => {
          if (block.type !== "text") {
            printBlock(block);
          }
        },
        onComplete: (result) => {
          saveMediaOutputs(result.outputs, result.interactionId, args.output as string | undefined);
          const latencySeconds = (performance.now() - startTime) / 1000;
          printCompletionSummary(result, latencySeconds);
          if (!args.json) {
            logRequest(result.interactionId, body);
            logResponse(result.interactionId, result);
          }
        },
      });
    }
  },
});
