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
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/interactions"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/operations"
	"github.com/spf13/cobra"
)

// transcribeFormats maps the --format values to the artifact extension.
var transcribeFormats = map[string]string{"md": ".md", "text": ".txt", "json": ".json", "srt": ".srt"}

// attachTranscribe turns the (claimed) transcribe command into the
// speech-to-text porcelain: a model interaction over an audio/video content
// block with a transcription prompt (schema-enforced structured output for
// json/srt), the transcript written to disk, absolute path on stdout.
func attachTranscribe(cmd *cobra.Command) {
	cmd.Use = "transcribe"
	cmd.Long = "Transcribe one or more audio or video inputs.\n\nPass each local path or uploaded files/<id> with --input. Inputs are validated\nbefore transcription, then processed sequentially. By default, stdout prints one\nabsolute artifact path per line. Formats: md (default), text, json, and srt\n(alias: captions). With multiple inputs, --out names a directory rather than a\nfile; an existing artifact is replaced. Requests are not stored server-side.\n\n" + inlineLimitNote
	cmd.Example = "  gemini-api transcribe -i interview.mp3\n" +
		"  gemini-api transcribe -i call.wav --format srt --out call.srt\n" +
		"  gemini-api transcribe -i files/abc123 --format json --no-speakers\n" +
		"  gemini-api transcribe -i a.mp3 -i b.mp3 --out ./transcripts/"
	helpMeta(cmd, "model "+defaultTranscribeModel+" · format md · speakers on · timestamps on",
		"https://ai.google.dev/gemini-api/docs/audio",
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
	s, sources, err := resolveMediaSources(cmd, inputs, transcribePolicy)
	if err != nil {
		return err
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
	for i, src := range sources {
		media, err := src.block(cmd)
		if err != nil {
			return fail(err)
		}
		body := newModelInteraction(model, media, textContentBlock(transcribePrompt(format, speakers, timestamps)))
		if structured {
			// Schema-enforced JSON; parseTranscript also tolerates fenced output.
			format := interactions.CreateCreateModelInteractionResponseFormatResponseFormat(
				interactions.CreateResponseFormatTextResponseFormat(interactions.TextResponseFormat{
					MimeType: interactions.TextResponseFormatMimeTypeApplicationJSON.ToPointer(),
					Schema:   transcriptSchema(speakers, timestamps),
				}))
			body.ResponseFormat = &format
		}
		req := operations.CreateInteractionRequest{
			Body: operations.CreateCreateInteractionRequestBodyCreateModelInteraction(body),
		}
		if isDryRun(cmd) {
			if _, err := s.Agent.Run(cmd.Context(), req, opts...); err != nil {
				return err
			}
			continue
		}

		progress(cmd, "Transcribing %s with %s...", src.label, model)
		res, err := s.Agent.Run(cmd.Context(), req, opts...)
		if err != nil {
			progress(cmd, "%s: request failed", inputRef(i+1, inputs[i]))
			return output.Error(cmd, fail(err))
		}
		text, err := interactionText(res.Interaction)
		if err != nil {
			return fail(err)
		}
		artifact, err := formatTranscriptArtifact(cmd, text, format, speakers, timestamps)
		if err != nil {
			return fail(err)
		}
		if err := writeArtifact(paths[i], []byte(artifact)); err != nil {
			return fail(runtimeError(fmt.Sprintf("cannot write %s: %v", paths[i], err)))
		}
		progress(cmd, "Wrote %s (%d bytes).", paths[i], len(artifact))
		result := map[string]any{
			"input": src.label, "path": paths[i], "size_bytes": len(artifact),
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
	return emitResult(cmd, strings.Join(paths, "\n"), map[string]any{
		"model": model, "format": format, "results": results,
	})
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

// parseTimestamp accepts "ss", "ss.mmm", "mm:ss", "mm:ss.mmm", "hh:mm:ss[.mmm]"
// (also with a comma decimal separator) and returns milliseconds. Only the
// last field may carry a fraction, and every field after the first must be
// below 60; the first is unbounded, so "125.5" and "90:15" are valid.
func parseTimestamp(s string) (int64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
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

func formatSRTTime(ms int64) string {
	h := ms / 3600000
	m := (ms % 3600000) / 60000
	sec := (ms % 60000) / 1000
	milli := ms % 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, sec, milli)
}
