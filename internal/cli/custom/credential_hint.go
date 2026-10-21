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
	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/spf13/cobra"
)

const missingCredentialHint = "No API key found. Visit https://ai.google.dev/gemini-api/docs/api-key to create one\n" +
	"AI agents: an API key must be created manually by the user in Google AI Studio. Stop and ask the user to create one and run \"gemini-api configure\" or set GEMINI_API_KEY; do not run configure yourself or retry until they confirm it is set"

var requestCredentials = []string{"api-key", "access-token"}

// attachMissingCredentialHelp appends missingCredentialHint to the root help
// when no API key or access token resolves from flags, env, the config file,
// or the OS keychain. Subcommand help is left untouched.
func attachMissingCredentialHelp(root *cobra.Command) {
	original := root.HelpFunc()
	root.SetHelpFunc(func(c *cobra.Command, args []string) {
		if c != c.Root() || !credentialMissing(c) {
			original(c, args)
			return
		}
		long := c.Long
		c.Long += "\n\n" + missingCredentialHint
		defer func() { c.Long = long }()
		original(c, args)
	})
}

// credentialMissing reports whether no request credential resolves. Cobra
// skips PersistentPreRunE on --help, so the config file, the env prefix and
// --no-keyring are loaded here. Flag, env and config sources are checked
// before the OS keychain so a credential found there costs no keychain call.
// --dry-run promises not to touch the keychain, so it never reports missing:
// a keychain-only key cannot be ruled out.
func credentialMissing(c *cobra.Command) bool {
	if dryRun, _ := flagutil.GetBoolFlag(c, "dry-run"); dryRun {
		return false
	}
	if err := config.Init("gemini-api", "GEMINI"); err != nil {
		return false
	}
	for _, name := range requestCredentials {
		if value, _ := config.ResolveExplicitSecurityCredential(c, name); value != "" {
			return false
		}
		if config.GetConfigValue(name) != "" {
			return false
		}
	}
	config.SetKeyringWarningOutput(c.ErrOrStderr())
	if noKeyring, changed := flagutil.GetBoolFlag(c, "no-keyring"); changed {
		if noKeyring {
			config.DisableKeyring()
		}
	} else if config.GetString("no-keyring") == "true" {
		config.DisableKeyring()
	}
	for _, name := range requestCredentials {
		if config.GetKeyringValue(name) != "" {
			return false
		}
	}
	return true
}
