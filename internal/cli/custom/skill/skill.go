// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package skill embeds the gemini-api-cli agent skill, vendored from
// github.com/google-gemini/gemini-skills. Refresh it with go generate.
package skill

import _ "embed"

const (
	Name   = "gemini-api-cli" // directory name; must equal the frontmatter name
	File   = "SKILL.md"
	Source = "https://github.com/google-gemini/gemini-skills/blob/main/skills/gemini-api-cli/SKILL.md"
)

//go:generate curl --fail --silent --show-error --location --output SKILL.md https://raw.githubusercontent.com/google-gemini/gemini-skills/main/skills/gemini-api-cli/SKILL.md

//go:embed SKILL.md
var Content []byte
