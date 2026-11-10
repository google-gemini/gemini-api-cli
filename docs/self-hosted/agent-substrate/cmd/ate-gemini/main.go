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

// The ate-gemini command integrates the Gemini self-hosted CLI runtime with Agent Substrate.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/google-gemini/gemini-api-cli/docs/self-hosted/agent-substrate/pkg/substrate"
)

var version = "dev"

func main() {
	if err := substrate.NewRootCommand(version).ExecuteContext(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
