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

// Package custom hosts hand-written commands that register into the
// generated CLI.
//
// This file is generated ONCE and is never overwritten on regeneration — it
// is yours to edit. Commands added here survive regeneration, may use the
// CLI's internal packages (auth, config, output formatting, dry-run
// plumbing), and can reshape the generated command tree: add new top-level
// intent commands, claim a shared name, or mount generated groups beneath a
// curated parent command.
package custom

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/usage"
	"github.com/spf13/cobra"
)

// Register is called after every generated command has been attached to the
// root command. It receives the fully-assembled root and may add, remove,
// wrap, or rearrange commands freely.
//
// Here it turns tier-1 commands the declarative intent layer cannot express
// into hand-written porcelain backed by the Interactions API (the CLI's own
// embedded SDK, plus a resumable uploader for the Files API):
//   - tts / analyze / transcribe are declared `custom: true` in the command
//     manifest; the generated intent layer attaches each one with its name,
//     category, and help-group position and leaves RunE to us.
//   - files upload is added under the generated files group (whose leaf
//     commands — list/get/delete/register — stay generated).
//   - files get / delete normalize "files/<id>" to the bare "<id>" path
//     parameter and reject malformed ids before sending a request.
//   - models get normalizes "models/<id>" to "<id>" and rejects malformed ids;
//     the generated models group's bare invocation stays the curated catalog
//     (merged in by the generated catalog layer), with list/get as its live API
//     leaves.
func Register(root *cobra.Command) {
	if err := register(root); err != nil {
		// A drifted command surface (a declared custom command that no longer
		// exists, a files group that lost its shape) must fail loudly at
		// startup rather than silently dropping porcelain behaviour.
		panic(fmt.Sprintf("custom command registration failed: %v", err))
	}
}

func register(root *cobra.Command) error {
	// tokens stays as the generated "planned" placeholder: count-tokens needs
	// the classic GenAI surface that this interactions-only build drops. tts,
	// analyze, and transcribe are declared custom and backed by real porcelain
	// over the Interactions API.
	claims := []struct {
		name   string
		attach func(*cobra.Command)
	}{
		{"tts", attachTTS},
		{"analyze", attachAnalyze},
		{"transcribe", attachTranscribe},
	}
	for _, c := range claims {
		cmd := findChild(root, c.name)
		if cmd == nil {
			return fmt.Errorf("expected custom command %q to attach to, but it is not registered", c.name)
		}
		c.attach(cmd)
	}

	files := findChild(root, "files")
	if files == nil {
		return fmt.Errorf("expected the generated files group to mount porcelain under")
	}
	if findChild(files, "upload") == nil {
		uploadCmd := newFilesUploadCmd()
		usage.MarkDynamic(uploadCmd)
		files.AddCommand(uploadCmd)
	}
	if err := normalizeIdentifier(findChild(files, "get"), "file", normalizeFilePositional); err != nil {
		return fmt.Errorf("files get: %w", err)
	}
	if err := normalizeIdentifier(findChild(files, "delete"), "file", normalizeFilePositional); err != nil {
		return fmt.Errorf("files delete: %w", err)
	}

	models := findChild(root, "models")
	if models == nil {
		return fmt.Errorf("expected the generated models group to mount porcelain under")
	}
	if err := normalizeIdentifier(findChild(models, "get"), "model", normalizeModelPositional); err != nil {
		return fmt.Errorf("models get: %w", err)
	}

	environments := findChild(root, "environments")
	if environments == nil {
		return fmt.Errorf("expected the generated environments group to mount porcelain under")
	}
	envFiles := findChild(environments, "files")
	if envFiles == nil {
		return fmt.Errorf("expected the generated environments files subgroup to mount porcelain under")
	}
	if findChild(envFiles, "upload") == nil {
		uploadCmd := newEnvironmentFilesUploadCmd()
		usage.MarkDynamic(uploadCmd)
		envFiles.AddCommand(uploadCmd)
	}
	if findChild(envFiles, "download") == nil {
		downloadCmd := newEnvironmentFilesDownloadCmd()
		usage.MarkDynamic(downloadCmd)
		envFiles.AddCommand(downloadCmd)
	}
	if err := normalizeEnvironmentFilesList(findChild(envFiles, "list")); err != nil {
		return fmt.Errorf("environments files list: %w", err)
	}
	if err := attachInteractionInputs(root); err != nil {
		return err
	}

	boundStdinReads(root)
	return nil
}

// normalizeEnvironmentID strips an optional "environments/" prefix and rejects
// empty or multi-segment values before they reach the URL builder.
func normalizeEnvironmentID(env string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(env), "environments/")
	if trimmed == "" || strings.Contains(trimmed, "/") {
		return "", fmt.Errorf("invalid environment id %q; expected environments/<id> or a bare id", strings.TrimSpace(env))
	}
	return trimmed, nil
}

const environmentRootSentinel = "__gemini_cli_env_root__"

// normalizeEnvironmentFilePath strips leading slashes and "./" prefixes from a
// snapshot file path so "--path /var/mail" or "--path ./src" resolves to a
// relative path, and rejects paths that are empty or root-only.
func normalizeEnvironmentFilePath(p string) (string, error) {
	trimmed := strings.TrimSpace(p)
	for strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "/") {
		trimmed = strings.TrimPrefix(trimmed, "./")
		trimmed = strings.TrimLeft(trimmed, "/")
	}
	if trimmed == "" || trimmed == "." {
		return "", fmt.Errorf("invalid path %q; expected a relative path inside the environment (e.g. \"src\")", strings.TrimSpace(p))
	}
	return trimmed, nil
}

func normalizeEnvironmentListPath(p string) string {
	trimmed := strings.TrimSpace(p)
	for strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "/") {
		trimmed = strings.TrimPrefix(trimmed, "./")
		trimmed = strings.TrimLeft(trimmed, "/")
	}
	if trimmed == "" || trimmed == "." {
		return environmentRootSentinel
	}
	return trimmed
}

// normalizeEnvironmentFilesList normalizes --environment and --path on
// "environments files list" before building the request URL. Missing or blank
// --environment values are left to the generated request builder, while missing,
// ".", or "/" --path values map to environmentRootSentinel to list the root.
func normalizeEnvironmentFilesList(cmd *cobra.Command) error {
	if cmd == nil {
		return fmt.Errorf("command is not registered")
	}
	envFlag := cmd.Flags().Lookup("environment")
	if envFlag == nil {
		return fmt.Errorf("flag --environment is missing on %q", cmd.Name())
	}
	pathFlag := cmd.Flags().Lookup("path")
	if pathFlag == nil {
		return fmt.Errorf("flag --path is missing on %q", cmd.Name())
	}
	pathFlag.Usage = "File or directory path relative to the workspace root (e.g. src); omit or pass . or / to list the root"

	original := cmd.RunE
	if original == nil {
		return fmt.Errorf("command %q has no RunE", cmd.Name())
	}
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if usageRequested(c) {
			return original(c, args)
		}
		rawEnv, err := c.Flags().GetString("environment")
		if err != nil {
			return err
		}
		if strings.TrimSpace(rawEnv) != "" {
			normEnv, err := normalizeEnvironmentID(rawEnv)
			if err != nil {
				return usageError(err.Error())
			}
			if normEnv != rawEnv {
				if err := c.Flags().Set("environment", normEnv); err != nil {
					return err
				}
			}
		}

		rawPath, err := c.Flags().GetString("path")
		if err != nil {
			return err
		}
		normPath := normalizeEnvironmentListPath(rawPath)
		if normPath != rawPath {
			if err := c.Flags().Set("path", normPath); err != nil {
				return err
			}
		}
		return original(c, args)
	}
	return nil
}

// normalizeFilePositional folds "files/<id>" or a bare id down to the bare id
// the Files API path parameter expects, and rejects empty or malformed ids
// before they reach the API: an empty segment would turn "files delete" into a
// request against the /files/ collection path.
func normalizeFilePositional(id string) (string, error) {
	_, bare, ok := normalizeFileID(id)
	if !ok {
		return "", fmt.Errorf("invalid file id %q; expected files/<id> or a bare id", strings.TrimSpace(id))
	}
	return bare, nil
}

// modelIDShape is a single path segment: it forbids empty ids, slashes (path
// traversal / list-shaped requests), and other characters that would corrupt
// the /{api_version}/models/{model} path. Rejecting "/" is deliberate: it also
// excludes other resource collections such as "tunedModels/<id>", which are
// outside the get/list-only Models surface this CLI exposes.
var modelIDShape = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// normalizeModelPositional strips an optional "models/" prefix so both
// "models get gemini-flash-latest" and "models get models/gemini-flash-latest"
// resolve to the bare id the path parameter expects, and rejects empty or
// malformed ids before they reach the API.
func normalizeModelPositional(id string) (string, error) {
	id = strings.TrimPrefix(strings.TrimSpace(id), "models/")
	if !modelIDShape.MatchString(id) {
		return "", fmt.Errorf("invalid model id %q; expected a model name like \"gemini-flash-latest\"", id)
	}
	return id, nil
}

// boundStdinReads keeps the stdin read deadline on in every mode. The
// generated pre-run enables it only in agent mode; any other caller that leaves
// a pipe open without writing to it (a test harness, a wrapper script) would
// otherwise block a body-reading command forever. An explicit "@-" still waits
// for EOF.
func boundStdinReads(root *cobra.Command) {
	original := root.PersistentPreRunE
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if original != nil {
			if err := original(cmd, args); err != nil {
				return err
			}
		}
		flagutil.SetStdinReadDeadline(true)
		return nil
	}
}

// findChild returns the direct child command answering to a name or alias.
func findChild(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name || c.HasAlias(name) {
			return c
		}
	}
	return nil
}

// normalizeIdentifier validates and normalizes a generated command's
// identifier flag, whichever form (positional or --<flag>) supplied it. It
// returns an error (rather than silently no-op'ing) when the command surface
// has drifted so Register fails loudly at startup.
func normalizeIdentifier(cmd *cobra.Command, flagName string, normalize func(string) (string, error)) error {
	if cmd == nil {
		return fmt.Errorf("cannot normalize %q: command is not registered", flagName)
	}
	if cmd.Flags().Lookup(flagName) == nil {
		return fmt.Errorf("cannot normalize %q on %q: flag --%s is missing", flagName, cmd.Name(), flagName)
	}
	original := cmd.RunE
	if original == nil {
		return fmt.Errorf("cannot normalize %q on %q: command has no RunE", flagName, cmd.Name())
	}
	usage.MarkDynamic(cmd)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if usageRequested(c) {
			return original(c, args)
		}
		// Fold the positional (incl. an interactive answer) into the flag and
		// enforce presence with the generated wording.
		if err := flagutil.ResolvePositionalFlag(c, args); err != nil {
			return err
		}
		if normalize != nil {
			raw, err := c.Flags().GetString(flagName)
			if err != nil {
				return err
			}
			norm, err := normalize(raw)
			if err != nil {
				return usageError(err.Error())
			}
			if norm != raw {
				if err := c.Flags().Set(flagName, norm); err != nil {
					return err
				}
			}
		}
		return original(c, args)
	}
	return nil
}
