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

package contract_test

import (
	"testing"

	"github.com/google-gemini/gemini-api-cli/internal/cli"
	"github.com/google-gemini/gemini-api-cli/internal/cli/custom/envruntime"
	"github.com/google-gemini/gemini-api-cli/internal/sdk"
)

// TestHandWrittenDefaultsMatchGenerated pins the values that hand-written
// code under internal/cli/custom duplicates from the generated CLI and SDK.
// Regeneration updates the generated side only, so a version bump that
// misses a hand-written copy would otherwise ship silently.
func TestHandWrittenDefaultsMatchGenerated(t *testing.T) {
	isolateCLIEnv(t)
	root, err := cli.NewRootCommand()
	if err != nil {
		t.Fatalf("NewRootCommand() failed: %v", err)
	}
	apiVersion := root.PersistentFlags().Lookup("api-version")
	if apiVersion == nil {
		t.Fatal("root command has no --api-version flag")
	}
	if apiVersion.DefValue != envruntime.DefaultAPIVersion {
		t.Errorf("envruntime.DefaultAPIVersion = %q, but the generated --api-version default is %q; update the hand-written default (used by environments connect/poll/serve and files upload)",
			envruntime.DefaultAPIVersion, apiVersion.DefValue)
	}
	if sdkVersion := sdk.New().SDKVersion; cli.Version != sdkVersion {
		t.Errorf("cli.Version = %q, but the generated SDK version is %q; the version command and the User-Agent sent by hand-written requests would disagree",
			cli.Version, sdkVersion)
	}
}
