import { defineCommand } from "citty";
import { globalFlags } from "../../lib/shared-args";
import * as fs from "node:fs";
import * as path from "node:path";
import * as yaml from "js-yaml";
import { execSync } from "node:child_process";

export default defineCommand({
  meta: {
    name: "init",
    description: `Scaffold new agent project.

Examples:
  gemini-api agents init my-agent
  gemini-api agents init my-agent --base-agent waverunner`,
  },
  args: {
    ...globalFlags,
    name: {
      type: "positional",
      description: "Name of the agent project",
      required: true,
    },
    "base-agent": {
      type: "string",
      description: "Base model to set in agent.yaml",
      default: "waverunner",
    },
    "from-template": {
      type: "string",
      description: "Git or GCS URL to scaffold from",
    },
  },
  run({ args }) {
    const name = args.name;
    const baseAgent = args["base-agent"];
    const fromTemplate = args["from-template"];
    const dryRun = args["dry-run"];

    if (!name || name.trim() === "") {
      console.error("Error: Agent name cannot be empty.");
      process.exit(1);
    }

    if (fs.existsSync(name)) {
      console.log(`Directory '${name}' already exists.`);
      return;
    }

    if (dryRun) {
      console.log(`[dry-run] Would create directory: ${name}`);
      console.log(`[dry-run] Would create directory: ${path.join(name, "workspace")}`);
      console.log(`[dry-run] Would create file: ${path.join(name, "agent.yaml")}`);
      console.log(`[dry-run] Would create file: ${path.join(name, "AGENTS.md")}`);
      if (fromTemplate) {
        console.log(`[dry-run] Would scaffold from template: ${fromTemplate}`);
      }
      return;
    }

    if (fromTemplate) {
      console.log(`Scaffolding from template: ${fromTemplate}...`);
      try {
        // Simple git clone for now if it looks like a git URL or github repo
        if (fromTemplate.startsWith("http") || fromTemplate.startsWith("git@")) {
          execSync(`source ~/.bash_profile && git clone ${fromTemplate} ${name}`, { stdio: "inherit" });
          console.log(`✓ Scaffolded from template ${fromTemplate}`);
          return;
        } else {
          console.error(`Error: Unsupported template URL format: ${fromTemplate}`);
          process.exit(1);
        }
      } catch (error) {
        console.error(`Error scaffolding from template: ${error}`);
        process.exit(1);
      }
    }

    fs.mkdirSync(name, { recursive: true });
    fs.mkdirSync(path.join(name, "workspace"), { recursive: true });
    fs.mkdirSync(path.join(name, "skills"), { recursive: true });

    const agentConfig = {
      id: name,
      base_agent: baseAgent,
      description: `Scaffolded agent ${name}`,
      tools: [{ type: "code_execution" }],
      environment: { enabled: true },
    };

    fs.writeFileSync(
      path.join(name, "agent.yaml"),
      yaml.dump(agentConfig),
      "utf-8"
    );

    const STARTER_AGENTS_MD = `# Agent Instructions

Describe your agent's behavior, personality, and capabilities here.
This file is uploaded to the agent environment and merged with system_instruction on the server.

## What This Agent Does

<!-- Describe the agent's purpose -->

## Rules

<!-- Define behavioral rules and constraints -->

`;

    fs.writeFileSync(
      path.join(name, "AGENTS.md"),
      STARTER_AGENTS_MD,
      "utf-8"
    );

    fs.writeFileSync(
      path.join(name, ".env"),
      `# Configurations for your agent
# Add your environment variables here, e.g.:
# KEY=VALUE
`,
      "utf-8"
    );

    console.log(`✓ Initialized agent project in ${name}/`);
    console.log();
    console.log(`  ${name}/`);
    console.log(`  ├── agent.yaml       # Agent configuration`);
    console.log(`  ├── AGENTS.md        # System instructions`);
    console.log(`  ├── .env             # Configurations`);
    console.log(`  ├── skills/          # Custom skills`);
    console.log(`  └── workspace/       # Files seeded into environment`);
    console.log();
    console.log(`Next: cd ${name} && gemini-api agents test --prompt "Hello"`);
  },
});
