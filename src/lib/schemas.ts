import { z } from "zod";

export const ToolSchema = z.object({
  type: z.enum([
    "code_execution",
    "google_search",
    "url_context",
    "computer_use",
    "mcp_server",
    "file_search",
    "google_maps",
    "retrieval",
    "function",
  ]),
}).passthrough();

export const EnvironmentSchema = z.object({
  enabled: z.boolean().optional(),
}).passthrough();

export const AgentConfigSchema = z.object({
  id: z.string(),
  base_agent: z.literal("waverunner").optional(),
  description: z.string().optional(),
  system_instruction: z.string().optional(),
  tools: z.array(ToolSchema).optional(),
  subagents: z.array(z.string()).optional(),
  environment: EnvironmentSchema.optional(),
  base_environment: z.string().optional(),
  metadata: z.record(z.string()).optional(),
}).strict();

export type AgentConfig = z.infer<typeof AgentConfigSchema>;

export const CLIContextSchema = z.object({
  apiKey: z.string(),
  baseUrl: z.string().url(),
});

export type CLIContext = z.infer<typeof CLIContextSchema>;
