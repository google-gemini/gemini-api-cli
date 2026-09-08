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

// Shared plumbing for the hand-written tier-1 porcelain commands (tts, analyze,
// transcribe, files upload). Everything here leans on the
// generated CLI's exported packages so the porcelain inherits the same
// credential resolution, --server-url, --timeout, --dry-run, --debug, output
// formatting, and agent-mode error envelope as the generated commands.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google-gemini/gemini-api-cli/internal/client"
	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/interactive"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/interactions"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/operations"
	"github.com/google-gemini/gemini-api-cli/internal/usage"
	"github.com/spf13/cobra"
)

// Curated model defaults for the porcelain commands. They mirror the Build
// Spec's tier-1 line-up and the reference CLI's model registry.
const (
	defaultTextModel       = "gemini-3.6-flash"
	defaultTranscribeModel = "gemini-3.6-flash"
	defaultTTSModel        = "gemini-3.1-flash-tts-preview"
	defaultTTSVoice        = "Kore"
)

// helpMeta feeds the generated compact help template: the "Defaults:" line
// (rendered between the examples and the flags) and the "Learn: … · escalate:
// …" footer line. The template itself adds the machine-interface and
// --help-global footer lines to every command, so porcelain never embeds
// footer prose in cmd.Example.
func helpMeta(cmd *cobra.Command, defaults, learn, escalate string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	for key, value := range map[string]string{
		"speakeasy_help_defaults": defaults,
		"speakeasy_help_learn":    learn,
		"speakeasy_help_escalate": escalate,
	} {
		if value != "" {
			cmd.Annotations[key] = value
		}
	}
}

// porcelainError is a CLI-originated failure carrying remediation lines. The
// generated output layer classifies it (output.Classify) and renders it once
// in whichever mode the caller asked for — the classified human diagnostic,
// the JSON envelope, or agent mode — so porcelain commands never print
// themselves; they only return.
type porcelainError struct {
	error
	hints []string
}

func (e porcelainError) CLIHints() []string { return e.hints }

func (e porcelainError) Unwrap() error { return e.error }

// usageError reports a CLI-level usage/validation problem: typed at this
// boundary as a usage failure (validation_error / CLI_VALIDATION, exit 2).
// The shared classifier already appends the --help/--usage pointer, so hints
// carry only what is specific to the failure.
func usageError(msg string, hints ...string) error {
	return porcelainError{error: flagutil.WithCLIValidation(errors.New(msg)), hints: hints}
}

// runtimeError reports a failure the CLI detected after a successful API
// exchange (empty audio, blocked response, an upload that never became
// ACTIVE, ...). Untyped, so the shared classifier files it as a runtime
// failure (exit 1) with the supplied hints; the message names what went wrong.
func runtimeError(msg string, hints ...string) error {
	return porcelainError{error: errors.New(msg), hints: hints}
}

// isDryRun reports whether --dry-run is active. Under dry-run the HTTP layer
// prints the would-be request to stderr and returns a synthetic empty
// response, so commands must return before interpreting response bodies.
func isDryRun(cmd *cobra.Command) bool {
	return client.IsDryRun(cmd)
}

// callOpts prepares the SDK call options shared by every porcelain request.
// Under --dry-run the synthetic empty response is never deserialized.
func callOpts(cmd *cobra.Command) ([]operations.Option, error) {
	opts, err := output.PrepareCallOpts(cmd)
	if err != nil {
		return nil, err
	}
	if isDryRun(cmd) {
		opts = append(opts, operations.WithSkipDeserialization())
	}
	return opts, nil
}

// progress prints a status line to stderr unless the caller asked for a
// machine-readable output format (spec §7: stderr is silent on success under
// the JSON envelope).
func progress(cmd *cobra.Command, format string, args ...any) {
	if wantsEnvelope(cmd) {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...)
}

// effectiveOutputFormat mirrors the generated output package's format
// resolution (flag > env/config > agent-mode default) but returns "" when
// none names a format: the porcelain then prints only the deliverable on
// stdout, which is what a human driving the command wants by default. Agent
// mode selects the structured envelope (TOON), matching the CLI-wide
// --agent-mode contract and the generated commands.
func effectiveOutputFormat(cmd *cobra.Command) string {
	if flagutil.FlagChanged(cmd, "output-format") {
		format, _ := flagutil.GetStringFlag(cmd, "output-format")
		return format
	}
	if val := config.GetString("output-format"); val != "" {
		return val
	}
	if output.IsAgentMode() {
		return "toon"
	}
	return ""
}

// wantsEnvelope reports whether the caller asked for the structured envelope
// (an explicit output format or a --jq expression) instead of the bare
// deliverable.
func wantsEnvelope(cmd *cobra.Command) bool {
	if effectiveOutputFormat(cmd) != "" {
		return true
	}
	return flagutil.FlagChanged(cmd, "jq")
}

// emitResult writes the command result. Deliverable-only mode prints the
// bare deliverable (text, integer, or absolute path) followed by a newline;
// any explicit --output-format or --jq renders the small snake_case envelope
// through the generated output package so json/yaml/toon/table and jq all
// behave exactly as on generated commands.
func emitResult(cmd *cobra.Command, deliverable string, envelope map[string]any) error {
	if isDryRun(cmd) {
		return nil
	}
	if !wantsEnvelope(cmd) {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), deliverable)
		return err
	}
	if effectiveOutputFormat(cmd) == "pretty" {
		// Pretty is the human format: keep the deliverable itself, the
		// envelope is for machines.
		jq, _ := flagutil.GetStringFlag(cmd, "jq")
		if jq == "" {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), deliverable)
			return err
		}
	}
	return output.Result(cmd, envelope)
}

// modelFlag registers the shared -m/--model override with its curated default.
func modelFlag(cmd *cobra.Command, def, route string) {
	cmd.Flags().StringP("model", "m", "", fmt.Sprintf("Override model (default: %s) — %s-capable models: https://ai.google.dev/gemini-api/docs/models", def, route))
}

// resolveModel returns the --model override or the curated default, stripped
// of any "models/" prefix (the SDK path parameter wants the bare id). It
// holds the override to the same id shape as "models get".
func resolveModel(cmd *cobra.Command, def string) (string, error) {
	model, _ := flagutil.GetStringFlag(cmd, "model")
	if strings.TrimSpace(model) == "" {
		model = def
	}
	id, err := normalizeModelPositional(model)
	if err != nil {
		return "", usageError("--model: " + err.Error())
	}
	return id, nil
}

// stringPtr / boolPtr are tiny helpers for the SDK's pointer-heavy request
// models.
func stringPtr(s string) *string { return &s }
func boolPtr(b bool) *bool       { return &b }

// textInput resolves a single text input from exactly one of: positional args,
// a --<fileFlag> file, or --<stdinFlag> stdin. More than one source is a usage
// error; the returned text is trimmed and non-empty (or "" when none supplied).
func textInput(cmd *cobra.Command, args []string, fileFlag, stdinFlag string) (string, error) {
	sources := 0
	if len(args) > 0 {
		sources++
	}
	filePath := ""
	if fileFlag != "" && flagutil.FlagChanged(cmd, fileFlag) {
		filePath, _ = flagutil.GetStringFlag(cmd, fileFlag)
		sources++
	}
	fromStdin := false
	if stdinFlag != "" {
		fromStdin, _ = flagutil.GetBoolFlag(cmd, stdinFlag)
		if fromStdin {
			sources++
		}
	}
	if sources > 1 {
		return "", usageError("provide the text once: as an argument, via --" + fileFlag + ", or via --" + stdinFlag + " (not several)")
	}
	switch {
	case filePath != "":
		data, err := os.ReadFile(filePath)
		if err != nil {
			return "", usageError(fmt.Sprintf("cannot read --%s %q: %v", fileFlag, filePath, err))
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			return "", usageError(fmt.Sprintf("--%s %q is empty", fileFlag, filePath))
		}
		return text, nil
	case fromStdin:
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", usageError(fmt.Sprintf("cannot read stdin: %v", err))
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			return "", usageError("stdin is empty")
		}
		return text, nil
	case len(args) > 0:
		text := strings.TrimSpace(strings.Join(args, " "))
		if text == "" {
			return "", usageError("text cannot be empty")
		}
		return text, nil
	}
	return "", nil
}

// textContentBlock builds a user text content block for an interaction input.
func textContentBlock(text string) interactions.Content {
	return interactions.CreateContentText(interactions.TextContent{Text: text})
}

// modelPtr wraps a bare model id in the interactions Model pointer the request
// models expect.
func modelPtr(model string) *interactions.Model {
	m := interactions.Model(model)
	return &m
}

// newModelInteraction is the request every porcelain command starts from: one
// non-streaming turn that is not stored server-side. The porcelain is a
// one-shot transformation — nothing continues or manages the interaction
// afterwards — so retaining the prompt, media, and output would serve no one.
func newModelInteraction(model string, content ...interactions.Content) interactions.CreateModelInteraction {
	return interactions.CreateModelInteraction{
		Model:  modelPtr(model),
		Stream: boolPtr(false),
		Store:  boolPtr(false),
		Input:  interactions.CreateInteractionsInputArrayOfContent(content),
	}
}

// interactionOutcome rejects an interaction whose status is a known
// non-success value, so partial or halted output is never delivered as a
// complete artifact. An omitted status, or one this CLI does not know, is
// accepted: the content checks that follow still fail closed on empty output.
func interactionOutcome(it *interactions.Interaction) error {
	if it == nil {
		return runtimeError("empty response from the API")
	}
	detail := ""
	if msg := firstInteractionError(it); msg != "" {
		detail = ": " + msg
	}
	switch it.Status {
	case interactions.InteractionStatusIncomplete, interactions.InteractionStatusBudgetExceeded:
		return runtimeError(fmt.Sprintf("the API stopped before the output was complete (status: %s)%s", it.Status, detail),
			"Nothing was written; shorten or split the input, or raise the limit via gemini-api agent run")
	case interactions.InteractionStatusFailed, interactions.InteractionStatusCancelled,
		interactions.InteractionStatusRequiresAction, interactions.InteractionStatusInProgress,
		interactions.InteractionStatusQueued:
		return runtimeError(fmt.Sprintf("the interaction did not complete (status: %s)%s", it.Status, detail))
	}
	return nil
}

// interactionText concatenates the model's text output from a completed
// (non-streaming) interaction. It reports platform errors, non-success
// statuses, and empty responses as errors so callers never print a partial or
// empty deliverable and exit 0.
func interactionText(it *interactions.Interaction) (string, error) {
	if err := interactionOutcome(it); err != nil {
		return "", err
	}
	// Prefer the top-level output_text when the API still returns it; current
	// revisions surface output only through the model-output steps below.
	if it.OutputText != nil && strings.TrimSpace(*it.OutputText) != "" {
		return *it.OutputText, nil
	}
	var sb strings.Builder
	for _, step := range it.Steps {
		if step.ModelOutputStep == nil {
			continue
		}
		for _, block := range step.ModelOutputStep.Content {
			if block.TextContent != nil {
				sb.WriteString(block.TextContent.Text)
			}
		}
	}
	text := sb.String()
	if strings.TrimSpace(text) == "" {
		if msg := firstInteractionError(it); msg != "" {
			return "", runtimeError("the API returned no text: "+msg,
				"Rephrase the request or supply different input")
		}
		msg := "the API returned no text"
		if it.Status != "" {
			msg += " (status: " + string(it.Status) + ")"
		}
		return "", runtimeError(msg)
	}
	return text, nil
}

// firstInteractionError returns the first platform error message recorded on an
// interaction, if any.
func firstInteractionError(it *interactions.Interaction) string {
	for _, e := range it.Errors {
		if e.Message != nil && strings.TrimSpace(*e.Message) != "" {
			return *e.Message
		}
	}
	return ""
}

// usageEnvelope flattens the interaction's usage metadata into the porcelain
// envelope. Nil when the API reported nothing.
func usageEnvelope(u *interactions.Usage) map[string]any {
	if u == nil {
		return nil
	}
	m := map[string]any{}
	if u.TotalInputTokens != nil {
		m["prompt_tokens"] = *u.TotalInputTokens
	}
	if u.TotalOutputTokens != nil {
		m["output_tokens"] = *u.TotalOutputTokens
	}
	if u.TotalThoughtTokens != nil {
		m["thoughts_tokens"] = *u.TotalThoughtTokens
	}
	if u.TotalTokens != nil {
		m["total_tokens"] = *u.TotalTokens
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// artifactPath resolves where a generated artifact lands: --out when given,
// otherwise ./<prefix>-<timestamp>-<rand><ext> in the working directory. An
// --out naming a directory (trailing separator, or an existing directory)
// receives that default-named file. The returned path is absolute (that is
// what stdout carries).
func artifactPath(cmd *cobra.Command, outFlag, prefix, ext string) (string, error) {
	out, _ := flagutil.GetStringFlag(cmd, outFlag)
	out = strings.TrimSpace(out)
	if out == "" || namesDirectory(out) {
		var rnd [3]byte
		_, _ = rand.Read(rnd[:])
		out = filepath.Join(out, fmt.Sprintf("%s-%d-%s%s", prefix, time.Now().UnixMilli(), hex.EncodeToString(rnd[:]), ext))
	} else if !strings.EqualFold(filepath.Ext(out), ext) {
		// Keep the artifact honest: the extension follows the bytes.
		if filepath.Ext(out) == "" {
			out += ext
		} else {
			out = strings.TrimSuffix(out, filepath.Ext(out)) + ext
		}
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", usageError(fmt.Sprintf("cannot resolve --%s %q: %v", outFlag, out, err))
	}
	return abs, nil
}

// namesDirectory reports whether an --out value refers to a directory rather
// than the artifact file itself: a trailing separator, or an existing directory.
func namesDirectory(out string) bool {
	if strings.HasSuffix(out, "/") || strings.HasSuffix(out, string(os.PathSeparator)) {
		return true
	}
	info, err := os.Stat(out)
	return err == nil && info.IsDir()
}

// writeArtifact writes data atomically (temp file + rename) creating parent
// directories as needed. An existing destination is replaced, and the file
// keeps os.CreateTemp's 0600 mode: artifacts may hold private speech or text.
func writeArtifact(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// emitUsageKDL delegates to the generated tree-wide usage contract. Claimed
// commands are marked dynamic, so the live Cobra surface and interactive
// argument declaration are the source of truth.
func emitUsageKDL(cmd *cobra.Command, w io.Writer) error {
	return usage.EmitSchema(cmd, w)
}

// usageRequested is the shared --usage gate: it wins over every other surface.
func usageRequested(cmd *cobra.Command) bool {
	return usage.UsageRequested(cmd)
}

// declareInteractive attaches the generated CLI's typed positional contract
// to hand-written porcelain. Registration-time errors are programming errors,
// so fail fast instead of silently losing prompting and live usage metadata.
func declareInteractive(cmd *cobra.Command, spec interactive.CommandSpec) {
	if err := interactive.Declare(cmd, spec); err != nil {
		panic(fmt.Sprintf("declare interactive inputs for %s: %v", cmd.CommandPath(), err))
	}
}

// annotatePromptFlag attaches the generated CLI's flag prompt contract with
// the same fail-fast semantics as declareInteractive.
func annotatePromptFlag(cmd *cobra.Command, name string, spec flagutil.PromptFlagSpec) {
	if err := flagutil.AnnotatePromptFlag(cmd, name, spec); err != nil {
		panic(fmt.Sprintf("declare interactive flag --%s for %s: %v", name, cmd.CommandPath(), err))
	}
}
