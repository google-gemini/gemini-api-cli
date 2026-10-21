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
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"
)

type countingKeyring struct {
	secrets map[string]string
	gets    int
}

func (k *countingKeyring) Get(_, key string) (string, error) {
	k.gets++
	if v, ok := k.secrets[key]; ok {
		return v, nil
	}
	return "", keyring.ErrNotFound
}

func (k *countingKeyring) Set(_, key, value string) error {
	k.secrets[key] = value
	return nil
}

func (k *countingKeyring) Delete(_, key string) error {
	delete(k.secrets, key)
	return nil
}

// TestMissingCredentialHintKeychain covers the keychain source the contract
// harness cannot reach, and pins that cheaper sources and --dry-run skip the
// keychain entirely.
func TestMissingCredentialHintKeychain(t *testing.T) {
	cases := []struct {
		name     string
		secrets  map[string]string
		env      map[string]string
		args     []string
		wantHint bool
		wantGets int
	}{
		{"keychain api key", map[string]string{"api-key": "k"}, nil, []string{"--help"}, false, 2},
		{"keychain access token", map[string]string{"access-token": "t"}, nil, []string{"--help"}, false, 3},
		{"empty keychain", nil, nil, []string{"--help"}, true, 3},
		{"env access token skips keychain", nil, map[string]string{"GEMINI_ACCESS_TOKEN": "t"}, []string{"--help"}, false, 0},
		{"dry-run skips keychain", map[string]string{"api-key": "k"}, nil, []string{"--dry-run", "--help"}, false, 0},
		{"no-keyring env skips keychain", map[string]string{"api-key": "k"}, map[string]string{"GEMINI_NO_KEYRING": "true"}, []string{"--help"}, true, 0},
		{"no-keyring flag skips keychain", map[string]string{"api-key": "k"}, nil, []string{"--no-keyring", "--help"}, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			for _, key := range []string{"GEMINI_API_KEY", "GEMINI_ACCESS_TOKEN", "GEMINI_NO_KEYRING"} {
				t.Setenv(key, "")
			}
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			config.Reset()
			t.Cleanup(config.Reset)
			secrets := map[string]string{}
			for k, v := range tc.secrets {
				secrets[k] = v
			}
			backend := &countingKeyring{secrets: secrets}
			config.SetKeyringBackend(backend)
			t.Cleanup(config.ResetKeyring)
			t.Cleanup(func() { config.SetKeyringWarningOutput(os.Stderr) })

			root := &cobra.Command{Use: "gemini-api", Long: "Root description.", Run: func(*cobra.Command, []string) {}}
			root.PersistentFlags().Bool("dry-run", false, "")
			root.PersistentFlags().Bool("no-keyring", false, "")
			root.PersistentFlags().String("api-key", "", "")
			root.PersistentFlags().String("access-token", "", "")
			root.AddCommand(&cobra.Command{Use: "files", Run: func(*cobra.Command, []string) {}})
			attachMissingCredentialHelp(root)
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tc.args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}

			if got := strings.Contains(out.String(), missingCredentialHint); got != tc.wantHint {
				t.Errorf("hint present = %v, want %v:\n%s", got, tc.wantHint, out.String())
			}
			if backend.gets != tc.wantGets {
				t.Errorf("keychain gets = %d, want %d", backend.gets, tc.wantGets)
			}
			if root.Long != "Root description." {
				t.Errorf("root Long not restored: %q", root.Long)
			}
		})
	}
}
