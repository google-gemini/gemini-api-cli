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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	cmd.Short = "Ask questions about images, audio, video, documents, text files, or YouTube URLs (" + defaultTextModel + ")"
	cmd.Long = "Ask a question about one or more images, audio files, videos, PDFs, CSV or text\nfiles, or YouTube URLs. Pass each source with --input: files/<id> uses the Files\nAPI and YouTube URLs go by URI. The question applies to every input; its default\nis \"" + defaultAnalyzeQuestion + "\" By default, stdout prints only the answer;\nnothing is stored. Video with a non-static --processing streams internally.\n\n" + inlineLimitNote
	cmd.Example = "  gemini-api analyze -i report.pdf \"Summarize the key findings\"\n" +
		"  gemini-api analyze -i photo.jpg\n" +
		"  gemini-api analyze -i files/abc123 \"List every speaker with timestamps\"\n" +
		"  gemini-api analyze -i https://youtu.be/dQw4w9WgXcQ \"What happens at 1:00?\"\n" +
		"  gemini-api analyze -i a.png -i b.png \"What changed between these?\""
	helpMeta(cmd, "model "+defaultTextModel+" · question \""+defaultAnalyzeQuestion+"\"",
		"https://aistudio.google.com/docs/image-understanding (images) · https://aistudio.google.com/docs/audio (audio) · https://aistudio.google.com/docs/video-understanding (video) · https://aistudio.google.com/docs/document-processing (documents)",
		"full request control via gemini-api agent run")
	cmd.Args = cobra.ArbitraryArgs
	cmd.Flags().StringArrayP("input", "i", nil, "Local path, files/<id>, or YouTube URL to analyze (repeatable)")
	cmd.Flags().String("mime-type", "", "Override the detected MIME type (one input only)")
	cmd.Flags().Bool("upload", false, uploadFlagHelp)
	cmd.Flags().String("processing", "", processingFlagHelp)
	cmd.Flags().String("resolution", "", resolutionFlagHelp)
	cmd.Flags().String("system", "", "System instruction to steer the analysis")
	cmd.Flags().Bool("stdin", false, "Read text to analyze from stdin to EOF (sent after the --input media, before the question)")
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
	extraText, err := flagTextInput(cmd, "", "stdin")
	if err != nil {
		return err
	}
	if len(inputs) == 0 && extraText == "" && len(args) == 0 {
		return output.UsageHelpError(cmd, errors.New("missing required flag --input (a local path, files/<id>, or YouTube URL)"))
	}
	if len(inputs) == 0 && extraText == "" {
		if _, err := classifyMediaInput(cmd, args[0], "", true); err == nil {
			return usageError(fmt.Sprintf("--input is required; %q is media to analyze, not a question", args[0]),
				fmt.Sprintf("Pass it with -i: gemini-api analyze -i %s [question]", args[0]))
		}
		return usageError("--input is required", "Or pipe text to analyze with --stdin")
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
	if extraText != "" {
		contents = append(contents, textContentBlock(extraText))
	}
	contents = append(contents, textContentBlock(question))

	body := newModelInteraction(model, contents...)
	if sys, _ := flagutil.GetStringFlag(cmd, "system"); strings.TrimSpace(sys) != "" {
		body.SystemInstruction = stringPtr(strings.TrimSpace(sys))
	}
	stream := streamsAnalysis(sources)
	if stream {
		body.Stream = boolPtr(true)
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
		if err != nil {
			return output.Error(cmd, err)
		}
		return nil
	}

	progress(cmd, "Analyzing %d input(s) with %s...", len(sources), model)
	res, err := s.Agent.Run(cmd.Context(), req, opts...)
	if err != nil {
		return output.Error(cmd, err)
	}
	interaction := res.Interaction
	if stream {
		if interaction, err = collectStreamedInteraction(cmd, res); err != nil {
			return err
		}
	}
	text, err := interactionText(interaction)
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
	if u := usageEnvelope(interaction.Usage); u != nil {
		envelope["usage"] = u
	}
	return emitResult(cmd, text, envelope)
}

// streamsAnalysis reports whether the request is sent as an SSE stream: a
// video input processed in a mode other than static runs long enough that a
// single unary response risks the connection and auth window. The stream is
// consumed internally; the command still prints one answer.
func streamsAnalysis(sources []*mediaSource) bool {
	for _, src := range sources {
		if src.class() == contentVideo && src.opts.agentic() {
			return true
		}
	}
	return false
}

// collectStreamedInteraction drains the event stream of a streaming create
// and returns the interaction its completed event carries, rebuilt as the
// full resource shape the text and usage readers expect. When the completed
// event omits model-output text steps, streamed text deltas are assembled
// into a model output step so interactionText still succeeds.
func collectStreamedInteraction(cmd *cobra.Command, res *operations.CreateInteractionResponse) (*interactions.Interaction, error) {
	es := res.InteractionSSEStreamEvent
	if es == nil {
		return nil, runtimeError("the API did not return an event stream")
	}
	defer es.Close()
	var completed *interactions.InteractionCompletedEvent
	var deltaText strings.Builder
	for es.Next() {
		ev := es.Value()
		if ev == nil {
			continue
		}
		if e := ev.GetDataError(); e != nil {
			if raw, err := json.Marshal(e); err == nil {
				return nil, output.Error(cmd, &output.StreamEventError{Body: string(raw)})
			}
			msg := "the API reported an error while streaming"
			if e.Error != nil && e.Error.Message != nil && strings.TrimSpace(*e.Error.Message) != "" {
				msg = *e.Error.Message
			}
			return nil, runtimeError(msg)
		}
		if sd := ev.GetDataStepDelta(); sd != nil {
			if td := sd.GetDeltaText(); td != nil {
				deltaText.WriteString(td.Text)
			}
		}
		if c := ev.GetDataInteractionCompleted(); c != nil {
			completed = c
		}
	}
	if err := es.Err(); err != nil {
		return nil, output.Error(cmd, err)
	}
	if completed == nil {
		return nil, runtimeError("the stream ended without an interaction.completed event")
	}
	it := completed.Interaction
	steps := it.Steps
	if !stepsHaveText(steps) && deltaText.Len() > 0 {
		steps = append(slices.Clone(steps), interactions.CreateStepModelOutput(interactions.ModelOutputStep{
			Content: []interactions.Content{textContentBlock(deltaText.String())},
		}))
	}
	full := &interactions.Interaction{
		ID:     stringPtr(it.ID),
		Status: interactions.InteractionStatus(it.Status),
		Steps:  steps,
		Usage:  it.Usage,
	}
	if it.Model != nil {
		full.Model = modelPtr(*it.Model)
	}
	return full, nil
}

func stepsHaveText(steps []interactions.Step) bool {
	for _, step := range steps {
		if step.ModelOutputStep == nil {
			continue
		}
		for _, block := range step.ModelOutputStep.Content {
			if block.TextContent != nil && strings.TrimSpace(block.TextContent.Text) != "" {
				return true
			}
		}
	}
	return false
}
