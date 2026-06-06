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

import { existsSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { parse as parseDotenv } from "dotenv";
import { ConfigError } from "./errors";
import { type AgentConfig, AgentConfigSchema } from "./schemas";
import { parseYaml } from "./yaml";

export interface LoadedAgent {
  config: AgentConfig;
  dir: string;
}

export interface LoadAgentOptions {
  envFile?: string;
}

async function loadEnvFile(path: string): Promise<Record<string, string>> {
  const envPath = resolve(path);
  if (!existsSync(envPath)) {
    throw new ConfigError(`Environment file not found: ${path}`);
  }
  try {
    return parseDotenv(await readFile(envPath, "utf-8"));
  } catch (error) {
    throw new ConfigError(`Failed to read environment file ${path}: ${(error as Error).message}`);
  }
}

function resolveEnvVarString(value: string, envFileVars: Record<string, string>): string {
  const names = new Set(
    [...value.matchAll(/\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g)].map((match) => match[1]),
  );
  let resolved = value;

  for (const name of names) {
    const envValue = envFileVars[name] ?? process.env[name];
    if (envValue === undefined) {
      throw new ConfigError(`Missing environment variable ${name}`);
    }
    resolved = resolved.replaceAll(`\${${name}}`, () => envValue);
  }

  return resolved;
}

function resolveEnvVars(value: unknown, envFileVars: Record<string, string>): unknown {
  if (typeof value === "string") {
    return resolveEnvVarString(value, envFileVars);
  }

  if (Array.isArray(value)) {
    return value.map((item) => resolveEnvVars(item, envFileVars));
  }

  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value).map(([key, child]) => [key, resolveEnvVars(child, envFileVars)]),
    );
  }

  return value;
}

export async function loadAgent(dir: string, options: LoadAgentOptions = {}): Promise<LoadedAgent> {
  const absDir = resolve(dir);
  const yamlPath = join(absDir, "agent.yaml");

  try {
    // Read file
    const raw = await readFile(yamlPath, "utf-8");
    const envFileVars = options.envFile ? await loadEnvFile(options.envFile) : {};
    const parsed = resolveEnvVars(parseYaml(raw), envFileVars);

    // Validate with Zod
    const result = AgentConfigSchema.safeParse(parsed);
    if (!result.success) {
      const errors = result.error.issues
        .map((i) => `  - ${i.path.join(".")}: ${i.message}`)
        .join("\n");
      throw new ConfigError(`Invalid agent.yaml:\n${errors}`);
    }

    return { config: result.data, dir: absDir };
  } catch (error) {
    if (error instanceof ConfigError) {
      throw error;
    }
    throw new ConfigError(`Failed to load agent.yaml: ${(error as Error).message}`);
  }
}
