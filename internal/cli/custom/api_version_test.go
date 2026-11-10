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

package custom

import (
	"testing"

	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/spf13/cobra"
)

// apiVersionCmd returns a command with --api-version registered with
// flagDefault and, when set is non-nil, set on the command line to *set.
func apiVersionCmd(t *testing.T, flagDefault string, set *string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("api-version", flagDefault, "")
	if set != nil {
		if err := cmd.Flags().Set("api-version", *set); err != nil {
			t.Fatalf("set --api-version: %v", err)
		}
	}
	return cmd
}

// withAPIVersionEnv initializes config as the CLI does, with no config file
// and GEMINI_API_VERSION set to env.
func withAPIVersionEnv(t *testing.T, env string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("GEMINI_API_VERSION", env)
	config.Reset()
	t.Cleanup(config.Reset)
	if err := config.Init("gemini-api", "GEMINI"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
}

func strPtr(s string) *string { return &s }

func TestResolveAPIVersion(t *testing.T) {
	tests := []struct {
		name string
		cmd  func(t *testing.T) *cobra.Command
		env  string
		want string
	}{
		{
			name: "nil command falls back to defaultAPIVersion",
			cmd:  func(*testing.T) *cobra.Command { return nil },
			want: defaultAPIVersion,
		},
		{
			name: "unregistered flag falls back to defaultAPIVersion",
			cmd:  func(*testing.T) *cobra.Command { return &cobra.Command{Use: "test"} },
			want: defaultAPIVersion,
		},
		{
			name: "registered flag default",
			cmd:  func(t *testing.T) *cobra.Command { return apiVersionCmd(t, "v9default", nil) },
			want: "v9default",
		},
		{
			name: "env/config beats the flag default",
			cmd:  func(t *testing.T) *cobra.Command { return apiVersionCmd(t, "v9default", nil) },
			env:  "v1env",
			want: "v1env",
		},
		{
			name: "explicit flag beats env/config",
			cmd:  func(t *testing.T) *cobra.Command { return apiVersionCmd(t, "v9default", strPtr("v1")) },
			env:  "v1env",
			want: "v1",
		},
		{
			name: "explicitly empty flag is returned for the caller to reject",
			cmd:  func(t *testing.T) *cobra.Command { return apiVersionCmd(t, "v9default", strPtr("")) },
			env:  "v1env",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withAPIVersionEnv(t, tc.env)
			if got := resolveAPIVersion(tc.cmd(t)); got != tc.want {
				t.Errorf("resolveAPIVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExpandedFilesURIFollowsAPIVersion(t *testing.T) {
	const base = "https://generativelanguage.googleapis.com/"
	tests := []struct {
		name string
		cmd  func(t *testing.T) *cobra.Command
		want string
	}{
		{
			name: "nil command uses defaultAPIVersion",
			cmd:  func(*testing.T) *cobra.Command { return nil },
			want: base + defaultAPIVersion + "/files/abc",
		},
		{
			// Previously ignored: the registered default was never read.
			name: "registered flag default",
			cmd:  func(t *testing.T) *cobra.Command { return apiVersionCmd(t, "v9default", nil) },
			want: base + "v9default/files/abc",
		},
		{
			name: "explicit flag is trimmed",
			cmd:  func(t *testing.T) *cobra.Command { return apiVersionCmd(t, "v9default", strPtr(" /v1/ ")) },
			want: base + "v1/files/abc",
		},
		{
			name: "explicitly empty flag falls back to defaultAPIVersion",
			cmd:  func(t *testing.T) *cobra.Command { return apiVersionCmd(t, "v9default", strPtr("")) },
			want: base + defaultAPIVersion + "/files/abc",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withAPIVersionEnv(t, "")
			if got := expandedFilesURI(tc.cmd(t), "files/abc"); got != tc.want {
				t.Errorf("expandedFilesURI() = %q, want %q", got, tc.want)
			}
		})
	}
}
