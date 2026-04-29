// gemini-api files subcommand group
// TODO: Implement — see tasks/task_11.md

import { defineCommand } from "citty";

export default defineCommand({
  meta: {
    name: "files",
    description: "Manage environment files: download",
  },
  subCommands: {
    download: () => import("./download").then((m) => m.default),
  },
});
