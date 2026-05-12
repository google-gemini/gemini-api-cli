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

import { z } from "zod";

export const ToolSchema = z.object({
  type: z.enum([
    "code_execution",
    "google_search",
    "url_context",
  ]),
}).passthrough();

const SourceSchema = z.discriminatedUnion("type", [
  z.object({
    type: z.literal("gcs"),
    source: z.string(),
    target: z.string(),
  }),
  z.object({
    type: z.literal("inline"),
    content: z.string(),
    target: z.string(),
  }),
  z.object({
    type: z.literal("github"),
    source: z.string(),
    target: z.string(),
  }),
]);

const ConfigSchema = z.object({
  sources: z.array(SourceSchema),
});

export const EnvironmentSchema = z.union([
  z.object({ enabled: z.boolean() }),
  z.object({ env_id: z.string() }),
  z.object({ config: ConfigSchema }),
]);

export const AgentConfigSchema = z.object({
  id: z.string(),
  base_agent: z.literal("antigravity-preview-05-2026").optional(),
  description: z.string().optional(),
  instructions: z.string().optional(),
  tools: z.array(ToolSchema).optional(),
  base_environment: z.union([z.string(), z.object({ config: ConfigSchema })]).optional(),
  sources: z.array(SourceSchema).optional(),
  environment: EnvironmentSchema.optional(),
}).strict();

export type AgentConfig = z.infer<typeof AgentConfigSchema>;

export const CLIContextSchema = z.object({
  apiKey: z.string(),
  baseUrl: z.string().url(),
});

export type CLIContext = z.infer<typeof CLIContextSchema>;
