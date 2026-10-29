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
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/interactions"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/operations"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/sdkerrors"
	"github.com/spf13/cobra"
)

// transcribeFormats maps the --format values to the artifact extension.
var transcribeFormats = map[string]string{"md": ".md", "text": ".txt", "json": ".json", "srt": ".srt"}

// attachTranscribe turns the (claimed) transcribe command into the
// speech-to-text porcelain: a model interaction over an audio/video content
// block using transcription_config on dedicated transcribe models (or a
// transcription prompt with schema-enforced structured output on general
// multimodal models), the transcript written to disk, absolute path on stdout.
func attachTranscribe(cmd *cobra.Command) {
	cmd.Use = "transcribe"
	cmd.Short = "Audio/video → text (timestamps, captions) (" + defaultTranscribeModel + ")"
	cmd.Long = "Transcribe one or more audio or video inputs.\n\nPass local paths or uploaded files/<id> with --input; inputs are validated, then\nprocessed in order. By default, stdout prints one absolute artifact path per line.\nFormats: md (default), text, json, srt (alias: captions). With multiple inputs,\n--out names a directory; existing artifacts are replaced. Requests are not stored\nserver-side.\n\n" + inlineLimitNote
	cmd.Example = "  gemini-api transcribe -i interview.mp3\n" +
		"  gemini-api transcribe -i call.wav --format srt --out call.srt\n" +
		"  gemini-api transcribe -i files/abc123 --format json --no-speakers\n" +
		"  gemini-api transcribe -i a.mp3 -i b.mp3 --out ./transcripts/"
	helpMeta(cmd, "model "+defaultTranscribeModel+" (video: "+defaultTextModel+") · format md · speakers on · timestamps on",
		"https://ai.google.dev/gemini-api/docs/transcribe",
		"full request control via gemini-api agent run")
	cmd.Args = cobra.NoArgs
	cmd.Flags().StringArrayP("input", "i", nil, "Local path or files/<id> to transcribe (repeatable)")
	cmd.Flags().String("format", "md", "Transcript format: md, text, json, srt (alias: captions)")
	cmd.Flags().Bool("no-speakers", false, "Do not label speakers")
	cmd.Flags().Bool("no-timestamps", false, "Do not include timestamps (ignored for srt)")
	cmd.Flags().String("out", "", "Output path (default: ./transcript-<unix-ms>-<random>.<ext>)")
	cmd.Flags().String("mime-type", "", "Override the detected MIME type (one input only)")
	modelFlag(cmd, defaultTranscribeModel, "transcription")
	annotatePromptFlag(cmd, "input", flagutil.PromptFlagSpec{Required: true, Kind: "string-array", Order: 0})
	cmd.RunE = runTranscribe
}

func runTranscribe(cmd *cobra.Command, args []string) error {
	if usageRequested(cmd) {
		return emitUsageKDL(cmd, cmd.OutOrStdout())
	}
	inputs, _ := cmd.Flags().GetStringArray("input")
	if len(inputs) == 0 {
		return output.UsageHelpError(cmd, errors.New("missing required flag --input (a local audio/video path or files/<id>)"))
	}
	format, _ := flagutil.GetStringFlag(cmd, "format")
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "captions" {
		format = "srt"
	}
	ext, ok := transcribeFormats[format]
	if !ok {
		return usageError(fmt.Sprintf("invalid --format %q", format), "Valid formats: md, text, json, srt")
	}
	noSpeakers, _ := flagutil.GetBoolFlag(cmd, "no-speakers")
	noTimestamps, _ := flagutil.GetBoolFlag(cmd, "no-timestamps")
	speakers, timestamps := !noSpeakers, !noTimestamps
	if format == "srt" {
		timestamps = true
	}
	structured := format == "json" || format == "srt"
	model, err := resolveModel(cmd, defaultTranscribeModel)
	if err != nil {
		return err
	}
	modelChanged := flagutil.FlagChanged(cmd, "model")
	s, sources, err := resolveMediaSources(cmd, inputs, transcribePolicy)
	if err != nil {
		return err
	}
	if modelChanged && isTranscribeModel(model) {
		for i, src := range sources {
			if class, _ := contentClassOf(src.mimeType); class == contentVideo {
				hints := []string{fmt.Sprintf("Omit --model or pass --model %s to transcribe video", defaultTextModel)}
				if src.mimeType == "video/webm" {
					hints = append(hints, webmAudioHint)
				}
				return usageError(
					fmt.Sprintf("%s: model %q only accepts audio inputs (detected %s)", inputRef(i+1, inputs[i]), model, src.mimeType),
					hints...,
				)
			}
		}
	}
	paths, err := transcribeArtifactPaths(cmd, sources, ext)
	if err != nil {
		return err
	}

	opts, err := callOpts(cmd)
	if err != nil {
		return err
	}

	results := make([]map[string]any, 0, len(sources))
	// fail names the artifacts already written, which the final result would
	// otherwise have carried, so a later failure does not orphan them. They
	// travel as hints so every error rendering (human, JSON envelope, agent
	// mode) carries them and stderr stays a single document.
	fail := func(err error) error {
		if len(results) == 0 {
			return err
		}
		hints := make([]string, 0, len(results))
		for _, done := range results {
			hints = append(hints, fmt.Sprintf("Completed before the failure: %s", done["path"]))
		}
		var hinted interface{ CLIHints() []string }
		if errors.As(err, &hinted) {
			hints = append(hints, hinted.CLIHints()...)
		}
		return porcelainError{error: err, hints: hints}
	}
	reportedModel := model
	allFallback := len(sources) > 0
	for i, src := range sources {
		media, err := src.block(cmd)
		if err != nil {
			return fail(err)
		}
		srcModel := transcribeModelForSource(model, modelChanged, src.mimeType)
		if srcModel == model {
			allFallback = false
		}
		var body interactions.CreateModelInteraction
		if isTranscribeModel(srcModel) {
			body = newModelInteraction(srcModel, media)
			body.GenerationConfig = &interactions.GenerationConfig{
				TranscriptionConfig: buildTranscriptionConfig(speakers, timestamps),
			}
		} else {
			body = newModelInteraction(srcModel, media, textContentBlock(transcribePrompt(format, speakers, timestamps)))
			if structured {
				// Schema-enforced JSON; parseTranscript also tolerates fenced output.
				rf := interactions.CreateCreateModelInteractionResponseFormatResponseFormat(
					interactions.CreateResponseFormatTextResponseFormat(interactions.TextResponseFormat{
						MimeType: interactions.TextResponseFormatMimeTypeApplicationJSON.ToPointer(),
						Schema:   transcriptSchema(speakers, timestamps),
					}))
				body.ResponseFormat = &rf
			}
		}
		req := operations.CreateInteractionRequest{
			Body: operations.CreateCreateInteractionRequestBodyCreateModelInteraction(body),
		}
		if isDryRun(cmd) {
			if src.mimeType == "" && !modelChanged && isTranscribeModel(srcModel) {
				// files.get is not sent under --dry-run, so routing can't see a video.
				progress(cmd, "[DRY-RUN] %s: if this file is a video, the live run uses %s with a transcription prompt instead.", src.label, defaultTextModel)
			}
			if _, err := s.Agent.Run(cmd.Context(), req, opts...); err != nil {
				return err
			}
			continue
		}

		if srcModel != model {
			note := fmt.Sprintf("%s is %s and %s accepts audio only; using %s.", src.label, src.mimeType, model, srcModel)
			if src.mimeType == "video/webm" {
				note += " " + webmAudioHint + "."
			}
			progress(cmd, "%s", note)
		}
		progress(cmd, "Transcribing %s with %s...", src.label, srcModel)
		res, err := s.Agent.Run(cmd.Context(), req, opts...)
		if err != nil {
			progress(cmd, "%s: request failed", inputRef(i+1, inputs[i]))
			return output.Error(cmd, fail(transcribeRequestError(err, srcModel, speakers, timestamps)))
		}
		artifact, err := formatTranscribeInteraction(cmd, res.Interaction, srcModel, format, speakers, timestamps)
		if err != nil {
			return fail(err)
		}
		if err := writeArtifact(paths[i], []byte(artifact)); err != nil {
			return fail(runtimeError(fmt.Sprintf("cannot write %s: %v", paths[i], err)))
		}
		progress(cmd, "Wrote %s (%d bytes).", paths[i], len(artifact))
		result := map[string]any{
			"input": src.label, "path": paths[i], "size_bytes": len(artifact), "model": srcModel,
		}
		if src.mimeType != "" {
			result["mime_type"] = src.mimeType
		}
		if u := usageEnvelope(res.Interaction.Usage); u != nil {
			result["usage"] = u
		}
		results = append(results, result)
	}
	if isDryRun(cmd) {
		return nil
	}
	if allFallback {
		reportedModel = defaultTextModel
	}
	return emitResult(cmd, strings.Join(paths, "\n"), map[string]any{
		"model": reportedModel, "format": format, "results": results,
	})
}

// isTranscribeModel reports whether model is a dedicated speech-to-text model
// (gemini-3.5-transcribe and its successors), which take audio plus
// generation_config.transcription_config rather than a transcription prompt.
// The substring match deliberately covers future *-transcribe releases and
// "models/"-prefixed names.
func isTranscribeModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "transcribe")
}

// transcribeModelForSource selects the effective model for an input source.
// Dedicated speech-to-text models (like gemini-3.5-transcribe) only accept
// audio inputs, so video inputs fall back to defaultTextModel unless --model
// was set explicitly.
func transcribeModelForSource(model string, modelChanged bool, mimeType string) string {
	if !modelChanged && isTranscribeModel(model) {
		if class, _ := contentClassOf(mimeType); class == contentVideo {
			return defaultTextModel
		}
	}
	return model
}

// webmAudioHint points audio-only WebM recordings, which the .webm extension
// maps to video/webm, at the dedicated transcribe model.
const webmAudioHint = "For an audio-only WebM recording, pass --mime-type audio/webm"

// transcribeRequestError adds remediation to a request a dedicated transcribe
// model rejected (HTTP 400) while diarization or word timestamps were on. The
// transcription docs limit those features to 30 minutes of audio; the live API
// has been seen to accept somewhat longer input, so the hint is conditional.
func transcribeRequestError(err error, model string, speakers, timestamps bool) error {
	if !isTranscribeModel(model) || (!speakers && !timestamps) || httpStatusOf(err) != http.StatusBadRequest {
		return err
	}
	return porcelainError{error: err, hints: []string{
		"If the recording is over 30 minutes (the documented limit for " + model + " with speaker labels or timestamps), split it, or re-run with --no-speakers --no-timestamps or --model " + defaultTextModel,
	}}
}

// httpStatusOf returns the HTTP status of an API error, or 0.
func httpStatusOf(err error) int {
	var client *sdkerrors.CreateInteractionClientError
	if errors.As(err, &client) && client.HTTPMeta.Response != nil {
		return client.HTTPMeta.Response.StatusCode
	}
	var invalid *sdkerrors.ResponseValidationError
	if errors.As(err, &invalid) {
		return invalid.StatusCode
	}
	var def *sdkerrors.SDKDefaultError
	if errors.As(err, &def) {
		return def.StatusCode
	}
	return 0
}

// buildTranscriptionConfig builds the Interactions API transcription_config
// for dedicated speech-to-text models: verbatim mode, with speaker diarization
// and word-level timestamps as requested.
func buildTranscriptionConfig(speakers, timestamps bool) *interactions.TranscriptionConfig {
	verbatim := interactions.VerbatimTranscriptionMode{}
	if speakers {
		verbatim.DiarizationMode = stringPtr("speaker")
	}
	if timestamps {
		verbatim.TimestampGranularities = []string{"word"}
	}
	mode := interactions.CreateTranscriptionConfigModeTranscriptionMode(
		interactions.CreateTranscriptionModeVerbatim(verbatim),
	)
	return &interactions.TranscriptionConfig{
		Mode: &mode,
	}
}

// formatTranscribeInteraction renders one completed interaction as the
// requested artifact. Dedicated transcribe models are rendered from their
// word_info annotations; general models from the prompt-shaped answer.
func formatTranscribeInteraction(cmd *cobra.Command, interaction *interactions.Interaction, model, format string, speakers, timestamps bool) (string, error) {
	if !isTranscribeModel(model) {
		text, err := interactionText(interaction)
		if err != nil {
			return "", err
		}
		return formatTranscriptArtifact(cmd, text, format, speakers, timestamps)
	}
	if err := interactionOutcome(interaction); err != nil {
		return "", err
	}
	if blocks := extractWordBlocks(interaction); len(blocks) > 0 {
		segments, err := segmentsFromWords(blocks, segmentOptions{
			speakers:   speakers,
			timestamps: timestamps,
			captions:   format == "srt",
		})
		if err != nil {
			return "", err
		}
		return formatSegmentsArtifact(segments, format, speakers, timestamps)
	}
	// No word_info: expected only when neither diarization nor timestamps was
	// requested. The plain transcript still serves md, text, and bare json.
	text, err := interactionText(interaction)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	switch {
	case format == "json" && !speakers && !timestamps:
		return formatSegmentsArtifact([]transcriptSegment{{Content: text}}, format, false, false)
	case format == "json" || format == "srt":
		return "", runtimeError(
			fmt.Sprintf("%s returned no word-level annotations, so the transcript cannot be rendered as %s", model, format),
			"Re-run with --format text, or pass --model "+defaultTextModel)
	default:
		if speakers || timestamps {
			progress(cmd, "Warning: %s returned no word-level annotations; the transcript has no speaker labels or timestamps.", model)
		}
		return text + "\n", nil
	}
}

// wordBlock is one text content block of a transcription response with the
// word_info annotations attached to it.
type wordBlock struct {
	text  string
	words []*interactions.WordInfo
}

// extractWordBlocks collects the word_info annotations of every model output
// text block, keeping each block's text so segments can be cut from it.
func extractWordBlocks(interaction *interactions.Interaction) []wordBlock {
	if interaction == nil {
		return nil
	}
	var blocks []wordBlock
	for _, step := range interaction.Steps {
		if step.ModelOutputStep == nil {
			continue
		}
		for _, content := range step.ModelOutputStep.Content {
			if content.TextContent == nil {
				continue
			}
			block := wordBlock{text: content.TextContent.Text}
			for _, ann := range content.TextContent.Annotations {
				if ann.WordInfo != nil {
					block.words = append(block.words, ann.WordInfo)
				}
			}
			if len(block.words) > 0 {
				blocks = append(blocks, block)
			}
		}
	}
	return blocks
}

// Segmentation thresholds for word_info transcripts. A new segment starts on a
// speaker change, after a long pause, or at a sentence end that follows a
// shorter pause or an already long segment. Captions are also capped so a cue
// stays readable on screen.
const (
	segmentPauseMs            = 2000 // always split on a silence this long
	segmentSentencePauseMs    = 500  // split at a sentence end after this pause
	segmentSentenceWords      = 12   // split at a sentence end once this long
	captionMaxMs              = 7000 // longest caption cue
	captionMaxBytes           = 84   // about two 42-character caption lines
	captionSentenceSlackMs    = 1500 // a sentence's last word may overrun the cue this much
	captionSentenceSlackBytes = 12   // ... and this many bytes (one short word)
)

type segmentOptions struct {
	speakers, timestamps, captions bool
}

// transcriptWord is one word_info annotation, resolved. span is the word's
// byte range in its block text, extended to the next word's start so trailing
// punctuation and spacing travel with it; spanOK is false when the block's
// indices are missing or inconsistent.
type transcriptWord struct {
	text           string
	speaker        string
	startMs, endMs int64
	span           [2]int
	spanOK         bool
}

// resolveBlockWords validates and resolves a block's words. Spans are only
// trusted when every word carries in-range, non-decreasing byte indices that
// fall on UTF-8 boundaries; otherwise the block falls back to word texts.
func resolveBlockWords(block wordBlock, wordBase int, opts segmentOptions, labels speakerLabels) ([]transcriptWord, error) {
	words := make([]transcriptWord, 0, len(block.words))
	spansOK := true
	prevStart := 0
	for i, w := range block.words {
		tw := transcriptWord{text: trimPtr(w.Text)}
		if tw.text == "" {
			continue
		}
		if opts.speakers {
			tw.speaker = labels.label(trimPtr(w.Speaker))
		}
		if opts.timestamps {
			startStr, endStr := trimPtr(w.StartOffset), trimPtr(w.EndOffset)
			if startStr == "" || endStr == "" {
				return nil, runtimeError(fmt.Sprintf("word %d is missing start_offset/end_offset", wordBase+i+1))
			}
			var err error
			if tw.startMs, err = parseTimestamp(startStr); err != nil {
				return nil, runtimeError(fmt.Sprintf("word %d: bad start_offset %q", wordBase+i+1, startStr))
			}
			if tw.endMs, err = parseTimestamp(endStr); err != nil {
				return nil, runtimeError(fmt.Sprintf("word %d: bad end_offset %q", wordBase+i+1, endStr))
			}
		}
		if w.StartIndex == nil || w.EndIndex == nil {
			spansOK = false
		} else {
			start, end := *w.StartIndex, *w.EndIndex
			if start < prevStart || start > end || end > len(block.text) || !runeBoundary(block.text, start) {
				spansOK = false
			}
			tw.span = [2]int{start, end}
			prevStart = start
		}
		words = append(words, tw)
	}
	if !spansOK || len(words) == 0 {
		return words, nil
	}
	// Each span runs to the next word's start (or the block end), and the
	// first absorbs any leading text, so the spans partition the block text.
	words[0].span[0] = 0
	for i := range words {
		end := len(block.text)
		if i+1 < len(words) {
			end = words[i+1].span[0]
		}
		words[i].span[1] = end
		words[i].spanOK = true
	}
	return words, nil
}

// segmentsFromWords groups word_info annotations into transcript segments.
// Segment text is cut from the response text by the words' byte indices, so
// the model's spacing, punctuation, and formatting survive (and languages
// written without spaces are not split apart); without usable indices the
// words are joined with spaces.
func segmentsFromWords(blocks []wordBlock, opts segmentOptions) ([]transcriptSegment, error) {
	var segments []transcriptSegment
	wordBase := 0
	labels := speakerLabels{}
	for _, block := range blocks {
		words, err := resolveBlockWords(block, wordBase, opts, labels)
		if err != nil {
			return nil, err
		}
		wordBase += len(block.words)
		var cur []transcriptWord
		flush := func() {
			if len(cur) == 0 {
				return
			}
			seg := transcriptSegment{Content: segmentText(block.text, cur)}
			if opts.speakers {
				seg.Speaker = cur[0].speaker
			}
			if opts.timestamps {
				seg.StartTime = formatTimestamp(cur[0].startMs)
				seg.EndTime = formatTimestamp(cur[len(cur)-1].endMs)
			}
			if seg.Content != "" {
				segments = append(segments, seg)
			}
			cur = nil
		}
		for _, w := range words {
			if len(cur) > 0 && shouldSplit(block.text, cur, w, opts) {
				flush()
			}
			cur = append(cur, w)
		}
		flush()
	}
	if len(segments) == 0 {
		return nil, runtimeError("transcription response has no segments")
	}
	return segments, nil
}

// shouldSplit reports whether next starts a new segment after cur.
func shouldSplit(text string, cur []transcriptWord, next transcriptWord, opts segmentOptions) bool {
	last := cur[len(cur)-1]
	if opts.speakers && next.speaker != cur[0].speaker {
		return true
	}
	pause := next.startMs - last.endMs
	if opts.timestamps && pause >= segmentPauseMs {
		return true
	}
	if opts.captions && opts.timestamps && captionFull(text, cur, next, opts) {
		return true
	}
	if !endsSentence(wordDisplay(text, last)) {
		return false
	}
	return !opts.speakers || opts.captions || (opts.timestamps && pause >= segmentSentencePauseMs) || len(cur) >= segmentSentenceWords
}

// captionFull reports whether adding next would overflow the caption cue
// that cur has started. The width counts the "Speaker N: " prefix the cue is
// rendered with. Captions also split at every sentence end (see shouldSplit),
// so a cue holds at most one sentence's tail; the word that ends that sentence
// may overflow the caps by a bounded amount (captionSentenceSlackMs and
// captionSentenceSlackBytes) rather than be stranded alone in the next cue.
func captionFull(text string, cur []transcriptWord, next transcriptWord, opts segmentOptions) bool {
	width := len(segmentText(text, append(cur[:len(cur):len(cur)], next)))
	if opts.speakers {
		width += len(cur[0].speaker) + len(": ")
	}
	duration := next.endMs - cur[0].startMs
	if endsSentence(wordDisplay(text, next)) {
		return duration > captionMaxMs+captionSentenceSlackMs || width > captionMaxBytes+captionSentenceSlackBytes
	}
	return duration > captionMaxMs || width > captionMaxBytes
}

// segmentText is the display text of a run of words.
func segmentText(text string, words []transcriptWord) string {
	if words[0].spanOK && words[len(words)-1].spanOK {
		return strings.TrimSpace(text[words[0].span[0]:words[len(words)-1].span[1]])
	}
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = w.text
	}
	return strings.Join(parts, " ")
}

// wordDisplay is a word as it appears in the text, trailing punctuation
// included when the span is known.
func wordDisplay(text string, w transcriptWord) string {
	if w.spanOK {
		return strings.TrimSpace(text[w.span[0]:w.span[1]])
	}
	return w.text
}

func runeBoundary(s string, i int) bool {
	return i == len(s) || (i >= 0 && i < len(s) && utf8.RuneStart(s[i]))
}

func trimPtr(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// speakerLabels numbers diarized speakers by first appearance ("Speaker 1",
// "Speaker 2", ...), matching the labels the prompt-based path produces. The
// API's raw labels are opaque (the live API returns zero-based "spk:0", while
// the reference documents "spk_1"), so they are only used as keys.
type speakerLabels map[string]string

func (l speakerLabels) label(raw string) string {
	if raw == "" {
		return "Speaker"
	}
	if label, ok := l[raw]; ok {
		return label
	}
	label := fmt.Sprintf("Speaker %d", len(l)+1)
	l[raw] = label
	return label
}

func endsSentence(word string) bool {
	word = strings.TrimRight(word, `"'”’)]`)
	return strings.HasSuffix(word, ".") || strings.HasSuffix(word, "?") || strings.HasSuffix(word, "!") ||
		strings.HasSuffix(word, "。") || strings.HasSuffix(word, "？") || strings.HasSuffix(word, "！")
}

// formatSegmentsArtifact renders word_info segments in the requested format.
func formatSegmentsArtifact(segments []transcriptSegment, format string, speakers, timestamps bool) (string, error) {
	switch format {
	case "json":
		pretty, _ := json.MarshalIndent(map[string]any{"segments": segments}, "", "  ")
		return string(pretty) + "\n", nil
	case "srt":
		artifact, err := renderSRT(segments, speakers)
		if err != nil {
			return "", runtimeError("cannot render SRT captions: "+err.Error(),
				"Re-run with --format json to keep the raw segments")
		}
		return artifact, nil
	}
	lines := make([]string, 0, len(segments))
	for _, seg := range segments {
		var prefix string
		switch {
		case speakers && timestamps:
			prefix = fmt.Sprintf("[%s - %s] %s:", seg.StartTime, seg.EndTime, seg.Speaker)
		case speakers:
			prefix = seg.Speaker + ":"
		case timestamps:
			prefix = fmt.Sprintf("[%s - %s]", seg.StartTime, seg.EndTime)
		}
		switch {
		case prefix == "":
			lines = append(lines, seg.Content)
		case format == "md":
			lines = append(lines, "**"+prefix+"** "+seg.Content)
		default:
			lines = append(lines, prefix+" "+seg.Content)
		}
	}
	if format == "md" {
		return strings.Join(lines, "\n\n") + "\n", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// formatTranscriptArtifact renders the model's answer in the requested format.
// Only srt checks the timestamps' ranges and order: a caption file with broken
// timing misleads, while json hands the raw segments over as the API gave them,
// which is also the way out when srt rejects a transcript.
func formatTranscriptArtifact(cmd *cobra.Command, text, format string, speakers, timestamps bool) (string, error) {
	switch format {
	case "json":
		segments, err := parseTranscript(text, speakers, timestamps)
		if err != nil {
			return "", err
		}
		pretty, _ := json.MarshalIndent(map[string]any{"segments": segments}, "", "  ")
		return string(pretty) + "\n", nil
	case "srt":
		segments, err := parseTranscript(text, speakers, true)
		if err != nil {
			return "", err
		}
		artifact, err := renderSRT(segments, speakers)
		if err != nil {
			return "", runtimeError("cannot render SRT captions: "+err.Error(),
				"Re-run with --format json to keep the raw segments")
		}
		return artifact, nil
	default:
		return strings.TrimRight(text, "\n") + "\n", nil
	}
}

var unsafeArtifactName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func transcribeArtifactPaths(cmd *cobra.Command, sources []*mediaSource, ext string) ([]string, error) {
	out, _ := flagutil.GetStringFlag(cmd, "out")
	out = strings.TrimSpace(out)
	paths := make([]string, len(sources))
	// A single input may still name a directory, which is never treated as the
	// artifact file itself.
	if out == "" || (len(sources) == 1 && !namesDirectory(out)) {
		for i := range sources {
			path, err := artifactPath(cmd, "out", "transcript", ext)
			if err != nil {
				return nil, err
			}
			paths[i] = path
		}
		return paths, nil
	}
	info, err := os.Stat(out)
	switch {
	case err == nil && !info.IsDir():
		return nil, usageError(fmt.Sprintf("--out %q must be a directory when transcribing multiple inputs", out))
	case err != nil && !os.IsNotExist(err):
		return nil, usageError(fmt.Sprintf("cannot inspect --out %q: %v", out, err))
	case os.IsNotExist(err) && isDryRun(cmd):
		// Dry-run previews the requests without touching the filesystem;
		// the paths are still computed so the preview names them.
	case os.IsNotExist(err):
		if err := os.MkdirAll(out, 0o755); err != nil {
			return nil, usageError(fmt.Sprintf("cannot create --out directory %q: %v", out, err))
		}
	}
	absDir, err := filepath.Abs(out)
	if err != nil {
		return nil, usageError(fmt.Sprintf("cannot resolve --out %q: %v", out, err))
	}
	for i, src := range sources {
		base := filepath.Base(src.label)
		base = strings.TrimSuffix(base, filepath.Ext(base))
		base = strings.Trim(unsafeArtifactName.ReplaceAllString(base, "-"), ".-_")
		if base == "" {
			base = "input"
		}
		paths[i] = filepath.Join(absDir, fmt.Sprintf("%s-%d%s", base, i+1, ext))
	}
	return paths, nil
}

// transcribePrompt builds the instruction for the requested shape.
func transcribePrompt(format string, speakers, timestamps bool) string {
	parts := []string{"Transcribe the speech in this recording accurately and completely."}
	if speakers {
		parts = append(parts, "Identify and label distinct speakers (Speaker 1, Speaker 2, ...).")
	} else {
		parts = append(parts, "Do not include speaker labels.")
	}
	if timestamps {
		parts = append(parts, "Include accurate timestamps (mm:ss or hh:mm:ss) for each segment.")
	} else {
		parts = append(parts, "Do not include timestamps.")
	}
	switch format {
	case "md":
		parts = append(parts, "Format the output as clean Markdown.")
	case "text":
		parts = append(parts, "Format the output as plain text.")
	case "json", "srt":
		parts = append(parts, "Return JSON matching the response schema: one segment per utterance.")
	}
	return strings.Join(parts, " ")
}

// transcriptSchema is the structured-output JSON schema for json/srt
// transcripts; its keys are the ones parseTranscript requires.
func transcriptSchema(speakers, timestamps bool) map[string]any {
	props := map[string]any{
		"content": map[string]any{"type": "string", "description": "Transcribed text of the segment"},
	}
	required := []string{"content"}
	if speakers {
		props["speaker"] = map[string]any{"type": "string", "description": "Speaker label, e.g. Speaker 1"}
		required = append(required, "speaker")
	}
	if timestamps {
		props["start_time"] = map[string]any{"type": "string", "description": "Segment start, mm:ss or hh:mm:ss(.mmm)"}
		props["end_time"] = map[string]any{"type": "string", "description": "Segment end, mm:ss or hh:mm:ss(.mmm)"}
		required = append(required, "start_time", "end_time")
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"segments": map[string]any{
				"type":        "array",
				"description": "Transcript segments in order",
				"items":       map[string]any{"type": "object", "properties": props, "required": required},
			},
		},
		"required": []string{"segments"},
	}
}

// transcriptSegment is one structured transcript entry.
type transcriptSegment struct {
	Speaker   string `json:"speaker,omitempty"`
	StartTime string `json:"start_time,omitempty"`
	EndTime   string `json:"end_time,omitempty"`
	Content   string `json:"content"`
}

// parseTranscript validates the model's structured JSON transcript.
func parseTranscript(raw string, speakers, timestamps bool) ([]transcriptSegment, error) {
	var payload struct {
		Segments []transcriptSegment `json:"segments"`
	}
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, runtimeError(fmt.Sprintf("transcription response is not valid JSON: %v", err))
	}
	if len(payload.Segments) == 0 {
		return nil, runtimeError("transcription response has no segments")
	}
	for i, seg := range payload.Segments {
		if strings.TrimSpace(seg.Content) == "" {
			return nil, runtimeError(fmt.Sprintf("segment %d has no content", i+1))
		}
		if timestamps && (seg.StartTime == "" || seg.EndTime == "") {
			return nil, runtimeError(fmt.Sprintf("segment %d is missing start_time/end_time", i+1))
		}
		if speakers && seg.Speaker == "" {
			payload.Segments[i].Speaker = "Speaker"
		}
	}
	return payload.Segments, nil
}

// renderSRT renders segments as SubRip captions. A segment that ends before it
// starts, or starts before its predecessor, is an error rather than a silent
// repair; overlapping segments (crosstalk) are fine.
func renderSRT(segments []transcriptSegment, speakers bool) (string, error) {
	var sb strings.Builder
	var prevStart int64
	for i, seg := range segments {
		start, err := parseTimestamp(seg.StartTime)
		if err != nil {
			return "", fmt.Errorf("segment %d: bad start_time %q", i+1, seg.StartTime)
		}
		end, err := parseTimestamp(seg.EndTime)
		if err != nil {
			return "", fmt.Errorf("segment %d: bad end_time %q", i+1, seg.EndTime)
		}
		if end < start {
			return "", fmt.Errorf("segment %d: end_time %q is before start_time %q", i+1, seg.EndTime, seg.StartTime)
		}
		if start < prevStart {
			return "", fmt.Errorf("segment %d: start_time %q is before the previous segment's", i+1, seg.StartTime)
		}
		prevStart = start
		// A blank line ends an SRT cue, so none may survive inside the text.
		line := blankLines.ReplaceAllString(strings.TrimSpace(seg.Content), "\n")
		if speakers && seg.Speaker != "" {
			line = seg.Speaker + ": " + line
		}
		fmt.Fprintf(&sb, "%d\n%s --> %s\n%s\n\n", i+1, formatSRTTime(start), formatSRTTime(end), line)
	}
	return sb.String(), nil
}

var (
	blankLines       = regexp.MustCompile(`\n\s*\n`)
	timestampWhole   = regexp.MustCompile(`^\d+$`)
	timestampSeconds = regexp.MustCompile(`^\d+(\.\d+)?$`)
)

// parseTimestamp accepts "ss", "ss.mmm", "ss[.mmm]s", "mm:ss", "mm:ss.mmm",
// "hh:mm:ss[.mmm]" (also with a comma decimal separator) and returns
// milliseconds. Only the last field may carry a fraction, and every field after
// the first must be below 60; the first is unbounded, so "125.5" and "90:15"
// are valid.
func parseTimestamp(s string) (int64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if !strings.Contains(s, ":") {
		s = strings.TrimSuffix(s, "s")
	}
	if s == "" {
		return 0, fmt.Errorf("empty timestamp")
	}
	fields := strings.Split(s, ":")
	if len(fields) > 3 {
		return 0, fmt.Errorf("too many fields")
	}
	var total float64
	for i, f := range fields {
		shape := timestampWhole
		if i == len(fields)-1 {
			shape = timestampSeconds
		}
		v, err := strconv.ParseFloat(f, 64)
		if err != nil || !shape.MatchString(f) {
			return 0, fmt.Errorf("invalid field %q", f)
		}
		if i > 0 && v >= 60 {
			return 0, fmt.Errorf("field %q is out of range", f)
		}
		total = total*60 + v
	}
	return int64(total*1000 + 0.5), nil
}

func formatTimestamp(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3600000
	m := (ms % 3600000) / 60000
	sec := (ms % 60000) / 1000
	milli := ms % 1000
	var base string
	if h > 0 {
		base = fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
	} else {
		base = fmt.Sprintf("%02d:%02d", m, sec)
	}
	if milli == 0 {
		return base
	}
	frac := strings.TrimRight(fmt.Sprintf("%03d", milli), "0")
	return base + "." + frac
}

func formatSRTTime(ms int64) string {
	h := ms / 3600000
	m := (ms % 3600000) / 60000
	sec := (ms % 60000) / 1000
	milli := ms % 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, sec, milli)
}
