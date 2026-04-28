import { resolve, join } from "node:path";
import { readFile } from "node:fs/promises";
import { parseYaml } from "./yaml";
import { AgentConfigSchema, type AgentConfig } from "./schemas";
import { ConfigError } from "./errors";

export interface LoadedAgent {
  config: AgentConfig;
  dir: string;
}

export async function loadAgent(dir: string): Promise<LoadedAgent> {
  const absDir = resolve(dir);
  const yamlPath = join(absDir, "agent.yaml");
  
  try {
    // Read file
    const raw = await readFile(yamlPath, "utf-8");
    const parsed = parseYaml(raw);
    
    // Validate with Zod
    const result = AgentConfigSchema.safeParse(parsed);
    if (!result.success) {
      const errors = result.error.issues.map(i => 
        `  - ${i.path.join(".")}: ${i.message}`
      ).join("\n");
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
