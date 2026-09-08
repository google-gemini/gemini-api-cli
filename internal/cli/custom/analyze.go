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
	"errors"
	"fmt"
	"strings"

	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/interactive"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/interactions"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/operations"
	"github.com/spf13/cobra"
)

const defaultAnalyzeQuestion = "Describe this file in detail."

// attachAnalyze turns the claimed analyze command into multimodal
// question-answering porcelain. Inputs and the question are deliberately
// separate: --input is repeatable media, while the positional is text.
func attachAnalyze(cmd *cobra.Command) {
	cmd.Use = "analyze [question]"
	cmd.Long = "Ask a question about one or more images, audio files, videos, PDFs, CSV or text\nfiles, or YouTube URLs.\n\nPass each media source separately with --input. files/<id> references use the\nFiles API, and YouTube URLs are passed by URI. The optional question applies to\nall inputs; its default is \"" + defaultAnalyzeQuestion + "\"\nBy default, stdout prints only the model's answer. The request is not stored\nserver-side.\n\n" + inlineLimitNote
	cmd.Example = "  gemini-api analyze -i report.pdf \"Summarize the key findings\"\n" +
		"  gemini-api analyze -i photo.jpg\n" +
		"  gemini-api analyze -i files/abc123 \"List every speaker with timestamps\"\n" +
		"  gemini-api analyze -i https://youtu.be/dQw4w9WgXcQ \"What happens at 1:00?\"\n" +
		"  gemini-api analyze -i a.png -i b.png \"What changed between these?\""
	helpMeta(cmd, "model "+defaultTextModel+" · question \""+defaultAnalyzeQuestion+"\"",
		"https://ai.google.dev/gemini-api/docs/image-understanding (images) · https://ai.google.dev/gemini-api/docs/audio (audio) · https://ai.google.dev/gemini-api/docs/video-understanding (video) · https://ai.google.dev/gemini-api/docs/document-processing (documents)",
		"full request control via gemini-api agent run")
	cmd.Args = cobra.ArbitraryArgs
	cmd.Flags().StringArrayP("input", "i", nil, "Local path, files/<id>, or YouTube URL to analyze (repeatable)")
	cmd.Flags().String("mime-type", "", "Override the detected MIME type (one input only)")
	cmd.Flags().String("system", "", "System instruction to steer the analysis")
	modelFlag(cmd, defaultTextModel, "text")
	annotatePromptFlag(cmd, "input", flagutil.PromptFlagSpec{Required: true, Kind: "string-array", Order: 0})
	declareInteractive(cmd, interactive.CommandSpec{Args: []interactive.ArgSpec{{
		Name: "question", Summary: "Question to ask (default: \"" + defaultAnalyzeQuestion + "\")", Variadic: true,
	}}})
	cmd.RunE = runAnalyze
}

func runAnalyze(cmd *cobra.Command, args []string) error {
	if usageRequested(cmd) {
		return emitUsageKDL(cmd, cmd.OutOrStdout())
	}
	inputs, _ := cmd.Flags().GetStringArray("input")
	if len(inputs) == 0 && len(args) == 0 {
		return output.UsageHelpError(cmd, errors.New("missing required flag --input (a local path, files/<id>, or YouTube URL)"))
	}
	if len(inputs) == 0 {
		return usageError("--input is required")
	}
	question := strings.TrimSpace(strings.Join(args, " "))
	if question == "" {
		question = defaultAnalyzeQuestion
	}
	model, err := resolveModel(cmd, defaultTextModel)
	if err != nil {
		return err
	}
	s, sources, err := resolveMediaSources(cmd, inputs, analyzePolicy)
	if err != nil {
		return err
	}

	contents := make([]interactions.Content, 0, len(sources)+1)
	labels := make([]string, 0, len(sources))
	mimeTypes := make([]string, 0, len(sources))
	var inlineBytes int64
	for _, src := range sources {
		block, err := src.block(cmd)
		if err != nil {
			return err
		}
		// Validation summed the sizes it saw; files may have grown since.
		if inlineBytes += src.inlineBytes; inlineBytes > maxInlineBytes {
			return usageError(fmt.Sprintf("%s: inline inputs grew past this CLI's %d MB request budget while the command ran", src.ref, maxInlineRequestBytes>>20))
		}
		contents = append(contents, block)
		labels = append(labels, src.label)
		if src.mimeType != "" {
			mimeTypes = append(mimeTypes, src.mimeType)
		}
	}
	contents = append(contents, textContentBlock(question))

	body := newModelInteraction(model, contents...)
	if sys, _ := flagutil.GetStringFlag(cmd, "system"); strings.TrimSpace(sys) != "" {
		body.SystemInstruction = stringPtr(strings.TrimSpace(sys))
	}
	req := operations.CreateInteractionRequest{
		Body: operations.CreateCreateInteractionRequestBodyCreateModelInteraction(body),
	}

	opts, err := callOpts(cmd)
	if err != nil {
		return err
	}
	if isDryRun(cmd) {
		_, err := s.Agent.Run(cmd.Context(), req, opts...)
		return err
	}

	progress(cmd, "Analyzing %d input(s) with %s...", len(sources), model)
	res, err := s.Agent.Run(cmd.Context(), req, opts...)
	if err != nil {
		return output.Error(cmd, err)
	}
	text, err := interactionText(res.Interaction)
	if err != nil {
		return err
	}
	text = strings.TrimRight(text, "\n")
	envelope := map[string]any{
		"model": model, "inputs": labels, "question": question, "text": text,
	}
	if len(mimeTypes) > 0 {
		envelope["mime_types"] = mimeTypes
	}
	if u := usageEnvelope(res.Interaction.Usage); u != nil {
		envelope["usage"] = u
	}
	return emitResult(cmd, text, envelope)
}
