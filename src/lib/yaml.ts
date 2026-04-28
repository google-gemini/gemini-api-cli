import yaml from "js-yaml";

export function parseYaml(content: string): unknown {
  return yaml.load(content);
}
