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
	"cmp"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google-gemini/gemini-api-cli/internal/client"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/interactive"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/interactions"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/operations"
	"github.com/spf13/cobra"
)

// attachTTS turns the (claimed) tts command into the text-to-speech porcelain:
// a model interaction requesting the audio response format with a speech
// config, the returned audio written to disk (raw PCM wrapped as WAV), the
// absolute path on stdout.
func attachTTS(cmd *cobra.Command) {
	cmd.Use = "tts [text]"
	cmd.Short = "Text to speech (" + defaultTTSModel + ")"
	cmd.Long = "Convert text to speech. Text is spoken verbatim: put delivery direction in --style\n" +
		"(or inline tags like <short pause>, <sigh>) and custom voice descriptions in\n" +
		"--voice-prompt or gemini-api voices create. Writes 24 kHz mono WAV (--out foo.mp3 is written as foo.wav;\n" +
		"An existing file is replaced). By default, stdout prints the artifact path.\n\n" +
		"Arguments:\n" +
		"  <text>  Verbatim text to speak (or use -f / --stdin)"
	cmd.Example = "  gemini-api tts \"Welcome to the show\"                    # → ./gemini-tts-<unix-ms>-<random>.wav\n" +
		"  gemini-api tts \"Look out!\" --style \"whispered urgently\" --voice Puck --out out.wav\n" +
		"  gemini-api tts \"Hello there\" --voice-prompt \"Warm, gravelly radio host\"\n" +
		"  gemini-api tts \"Alice [excited]: hi! Bob: hello.\" --multi-speaker \"Alice=Kore,Bob=Puck\""
	helpMeta(cmd, "model "+defaultTTSModel+" · voice "+defaultTTSVoice+" · 24kHz mono WAV",
		"https://ai.google.dev/gemini-api/docs/speech-generation",
		"full request control via gemini-api agent run")
	cmd.Args = cobra.ArbitraryArgs
	cmd.Flags().StringP("voice", "v", defaultTTSVoice, "Voice name or custom voice_<id> for single-speaker audio (e.g. Kore, Puck, Zephyr, voice_abc123)")
	cmd.Flags().String("voice-prompt", "", "Design a custom voice from a text description before synthesizing (creates a reusable voice_<id>)")
	cmd.Flags().String("style", "", "Delivery style or acting direction (e.g. \"whispered urgently\"); text is spoken verbatim")
	cmd.Flags().String("multi-speaker", "", "Speaker→voice map for two-speaker scripts, e.g. \"Alice=Kore,Bob=Puck\"; start each turn with \"Speaker:\" or \"Speaker [style]:\"")
	cmd.Flags().StringP("file", "f", "", "Read the input text from a file")
	cmd.Flags().Bool("stdin", false, "Read the input text from stdin")
	cmd.Flags().String("out", "", "Output file or directory; the extension follows the audio (default: ./gemini-tts-<unix-ms>-<random>.wav)")
	cmd.Flags().String("language", "", "BCP-47 language code hint for the speech (e.g. en-US)")
	modelFlag(cmd, defaultTTSModel, "tts")
	declareInteractive(cmd, interactive.CommandSpec{Args: []interactive.ArgSpec{{
		Name: "text", Summary: "Text to speak", Required: true, Variadic: true,
		SatisfiedBy: []string{"file", "stdin"},
	}}})
	cmd.RunE = runTTS
}

func runTTS(cmd *cobra.Command, args []string) error {
	if usageRequested(cmd) {
		return emitUsageKDL(cmd, cmd.OutOrStdout())
	}
	text, err := textInput(cmd, args, "file", "stdin")
	if err != nil {
		return err
	}
	if text == "" {
		return output.UsageHelpError(cmd, errors.New("missing required input text (pass it as an argument, --file, or --stdin)"))
	}

	model, err := resolveModel(cmd, defaultTTSModel)
	if err != nil {
		return err
	}
	style, _ := flagutil.GetStringFlag(cmd, "style")
	style = strings.TrimSpace(style)
	voicePrompt, _ := flagutil.GetStringFlag(cmd, "voice-prompt")
	voicePrompt = strings.TrimSpace(voicePrompt)
	if flagutil.FlagChanged(cmd, "voice-prompt") && voicePrompt == "" {
		return usageError("--voice-prompt cannot be empty")
	}
	if voicePrompt != "" {
		if flagutil.FlagChanged(cmd, "voice") {
			return usageError("--voice and --voice-prompt are mutually exclusive",
				"Use \"gemini-api voices create --base-voice <voice>\" to remix an existing voice")
		}
		if spec, _ := flagutil.GetStringFlag(cmd, "multi-speaker"); strings.TrimSpace(spec) != "" {
			return usageError("--voice-prompt and --multi-speaker are mutually exclusive",
				"Create custom voices with \"gemini-api voices create\" and pass their voice_<id> in --multi-speaker")
		}
		if isLegacyMultiSpeakerModel(model) {
			return usageError(fmt.Sprintf("--voice-prompt is not supported by %s (custom voice design requires %s)", model, defaultTTSModel))
		}
	}

	speech, voiceLabel, err := buildSpeechConfig(cmd)
	if err != nil {
		return err
	}
	input, absent, dirWarnings, err := ttsInputWithStyle(model, text, style, speech)
	if err != nil {
		return err
	}
	s, err := client.NewClient(cmd)
	if err != nil {
		return err
	}
	opts, err := callOpts(cmd)
	if err != nil {
		return err
	}

	var designedVoiceID string
	if voicePrompt != "" {
		lang, _ := flagutil.GetStringFlag(cmd, "language")
		designedVoiceID, err = designVoiceForTTS(cmd, model, voicePrompt, strings.TrimSpace(lang))
		if err != nil {
			return err
		}
		if len(speech.ArrayOfSpeechConfig) > 0 {
			speech.ArrayOfSpeechConfig[0].Voice = stringPtr(designedVoiceID)
		}
		voiceLabel = designedVoiceID
	}

	// The format stays bare: the TTS models reject every explicit mime_type
	// and delivery ("not supported for models/…"), and answer with 24 kHz L16.
	responseFormat := interactions.CreateCreateModelInteractionResponseFormatResponseFormat(
		interactions.CreateResponseFormatAudioResponseFormat(interactions.AudioResponseFormat{}))
	// Warnings reach a human on stderr and a machine in the result envelope;
	// structured stderr stays reserved for the error document.
	var warnings []string
	// The API does not check names against the script; a stray one is
	// silently voiced wrong.
	for _, speaker := range absent {
		warnings = append(warnings, fmt.Sprintf("speaker %q does not appear in the text", speaker))
	}
	warnings = append(warnings, dirWarnings...)
	for _, warning := range warnings {
		progress(cmd, "warning: %s", warning)
	}
	body := newModelInteraction(model, input...)
	body.ResponseFormat = &responseFormat
	body.GenerationConfig = &interactions.GenerationConfig{SpeechConfig: speech}
	req := operations.CreateInteractionRequest{
		Body: operations.CreateCreateInteractionRequestBodyCreateModelInteraction(body),
	}

	if isDryRun(cmd) {
		_, err := s.Agent.Run(cmd.Context(), req, opts...)
		return err
	}

	progress(cmd, "Synthesizing speech with %s (voice %s)...", model, voiceLabel)
	res, err := s.Agent.Run(cmd.Context(), req, opts...)
	if err != nil {
		return output.Error(cmd, err)
	}
	audio, mimeType, channels, sampleRate, err := extractAudio(res.Interaction)
	if err != nil {
		return err
	}

	// Prefer the channel/rate the API reported on the audio block; fall back to
	// the MIME parameters, then to the 24 kHz mono the TTS models emit.
	format := parseAudioMIME(mimeType)
	if channels == 0 {
		channels = format.channels
	}
	if sampleRate == 0 {
		sampleRate = format.sampleRate
	}
	data := audio
	if format.isPCM {
		if channels == 0 {
			channels = 1
		}
		if sampleRate == 0 {
			sampleRate = 24000
		}
		data = append(wavHeader(len(audio), sampleRate, channels), audio...)
	}
	ext, ok := extensionForAudioMIME(mimeType)
	if !ok {
		return runtimeError(fmt.Sprintf("the API returned audio as %s, which cannot be written as a playable file", mimeType))
	}
	path, err := artifactPath(cmd, "out", "gemini-tts", ext)
	if err != nil {
		return err
	}
	if err := writeArtifact(path, data); err != nil {
		return runtimeError(fmt.Sprintf("cannot write %s: %v", path, err))
	}
	progress(cmd, "Wrote %s (%d bytes).", path, len(data))

	envelope := map[string]any{
		"model":      model,
		"path":       path,
		"mime_type":  mimeType,
		"size_bytes": len(data),
	}
	if designedVoiceID != "" {
		envelope["voice_id"] = designedVoiceID
	}
	if format.isPCM {
		envelope["sample_rate"] = sampleRate
		envelope["channels"] = channels
	}
	if len(warnings) > 0 {
		envelope["warnings"] = warnings
	}
	if u := usageEnvelope(res.Interaction.Usage); u != nil {
		envelope["usage"] = u
	}
	return emitResult(cmd, path, envelope)
}

// containsWord reports whether text names word on its own rather than inside a
// longer one ("Ann" is absent from "Annual").
func containsWord(text, word string) bool {
	edge := `[^\p{L}\p{M}\p{N}]`
	return regexp.MustCompile(`(^|` + edge + `)` + regexp.QuoteMeta(word) + `($|` + edge + `)`).MatchString(text)
}

// isLegacyMultiSpeakerModel reports whether model takes a multi-speaker script
// as one plain text block and rejects speech_metadata annotations ("not
// supported for this model"). Every other model rejects that block and needs
// one annotated block per turn.
func isLegacyMultiSpeakerModel(model string) bool {
	return strings.HasPrefix(model, "gemini-2.5-") || strings.HasPrefix(model, "gemini-3.1-")
}

// ttsInput builds the interaction input: the whole text as one block, or, for a
// multi-speaker script on a current model, one block per speaker turn annotated
// with its speaker. It also returns the declared speakers the script never
// voices.
func ttsInput(model, text string, speech *interactions.SpeechConfigUnion) ([]interactions.Content, []string, error) {
	input, absent, _, err := ttsInputWithStyle(model, text, "", speech)
	return input, absent, err
}

func ttsInputWithStyle(model, text, style string, speech *interactions.SpeechConfigUnion) ([]interactions.Content, []string, []string, error) {
	style = strings.TrimSpace(style)
	if style != "" && isLegacyMultiSpeakerModel(model) {
		return nil, nil, nil, usageError(fmt.Sprintf("--style is not supported by %s (speech_metadata requires %s)", model, defaultTTSModel))
	}
	if speech.SpeakerConfig == nil {
		var dirWarnings []string
		if !isLegacyMultiSpeakerModel(model) {
			dirWarnings = detectDirectionWarnings([]string{text})
		}
		if style == "" {
			return []interactions.Content{textContentBlock(text)}, nil, dirWarnings, nil
		}
		return []interactions.Content{interactions.CreateContentText(interactions.TextContent{
			Text: text,
			Annotations: []interactions.Annotation{interactions.CreateAnnotationSpeechMetadata(
				interactions.SpeechAnnotation{Style: stringPtr(style)})},
		})}, nil, dirWarnings, nil
	}
	speakers := make([]string, 0, len(speech.SpeakerConfig.Speakers))
	for _, sp := range speech.SpeakerConfig.Speakers {
		speakers = append(speakers, *sp.Speaker)
	}
	var absent []string
	if isLegacyMultiSpeakerModel(model) {
		for _, speaker := range speakers {
			if !containsWord(text, speaker) {
				absent = append(absent, speaker)
			}
		}
		return []interactions.Content{textContentBlock(text)}, absent, nil, nil
	}
	turns, err := speakerTurns(text, speakers)
	if err != nil {
		return nil, nil, nil, err
	}
	voiced := map[string]bool{}
	input := make([]interactions.Content, 0, len(turns))
	spokenTexts := make([]string, 0, len(turns))
	for _, turn := range turns {
		voiced[turn.speaker] = true
		spokenTexts = append(spokenTexts, turn.text)
		ann := interactions.SpeechAnnotation{Speaker: stringPtr(turn.speaker)}
		if turnStyle := cmp.Or(turn.style, style); turnStyle != "" {
			ann.Style = stringPtr(turnStyle)
		}
		input = append(input, interactions.CreateContentText(interactions.TextContent{
			Text:        turn.text,
			Annotations: []interactions.Annotation{interactions.CreateAnnotationSpeechMetadata(ann)},
		}))
	}
	for _, speaker := range speakers {
		if !voiced[speaker] {
			absent = append(absent, speaker)
		}
	}
	return input, absent, detectDirectionWarnings(spokenTexts), nil
}

var (
	stageDirectionHeaderRe = regexp.MustCompile(`(?im)(\b(director'?s\s+notes|audio\s+profile|scene\s+description|voice\s+design|voice\s+prompt)\b|^\s*#{1,6}\s+(director|transcript|audio|voice|scene|style|script)\b|^\s*(style|tone|delivery|voice|pacing|accent|emotion|persona|direction)\s*:|^\s*(say|read|speak|narrate)\s+(this\s+)?(in\s+a\b|with\s+a\b|[a-z]+ly\b)[^:\n]{0,60}:)`)
	bracketStageTagRe      = regexp.MustCompile(`(?i)\[(sighs?|laughs?|chuckles?|giggles?|gasps?|panting|yawns?|coughs?|clears\s+throat|sniffs?|groans?|whispers?|whispering|shouts?|shouting|yells?|yelling|crying|sobs?|applause|laughter|music|pause|short\s+pause|long\s+pause|beat|excited|sadly|angrily|softly|slowly|quickly|cheerfully|sarcastically|dramatically)\]`)
)

// detectDirectionWarnings checks spoken text on non-legacy TTS models for
// stage-direction headers or bracketed vocal tags that Gemini 3.8 TTS reads
// aloud instead of interpreting as acting instructions.
func detectDirectionWarnings(texts []string) []string {
	var hasHeader, hasBracketTag bool
	for _, t := range texts {
		if !hasHeader && stageDirectionHeaderRe.MatchString(t) {
			hasHeader = true
		}
		if !hasBracketTag && bracketStageTagRe.MatchString(t) {
			hasBracketTag = true
		}
	}
	var warnings []string
	if hasHeader {
		warnings = append(warnings, "input text looks like stage direction that Gemini 3.8 TTS speaks verbatim; pass delivery style via --style and voice descriptions via --voice-prompt")
	}
	if hasBracketTag {
		warnings = append(warnings, "square-bracket stage tags like [...] may be spoken aloud; use angle-bracket vocal tags (e.g. <sigh>, <laugh>, <short pause>) or --style")
	}
	return warnings
}

// isWordRune reports whether r belongs to a word. Combining marks count: in
// "श्रीराम" the vowel sign before "राम" is part of the word, not a boundary.
func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r) }

func isLineBreak(r rune) bool {
	return r == '\n' || r == '\r' || r == '\u0085' || r == '\u2028' || r == '\u2029'
}

func isHorizontalSpace(r rune) bool {
	return !isLineBreak(r) && (unicode.IsSpace(r) || unicode.In(r, unicode.Z))
}

// isMarkerRune reports whether r can make up a list or quote marker: neither a
// letter, a digit, nor a space.
func isMarkerRune(r rune) bool {
	return !isWordRune(r) && !unicode.IsSpace(r) && !unicode.In(r, unicode.Z)
}

// lineMarkerStart returns where the list or quote marker opening the line of
// the label at start begins ("- Alice:", "> Alice:", "“Alice:", "1. Alice:"),
// so the marker goes with the label instead of the previous turn or the text
// before the first label. Without one, it returns start: punctuation or a
// number after words or a label on the same line ("hi. - Bob:",
// "Alice: 42. Bob:", "Alice: ... Bob:") stays in the previous turn. The scan
// stops at floor, the end of the previous label, so a name made of marker
// runes ("😀: 🤖:") is never claimed as the next label's marker; the line
// start is still judged on the whole text.
func lineMarkerStart(text string, floor, start int) int {
	before := strings.TrimRightFunc(text[floor:start], isHorizontalSpace)
	rest := strings.TrimRightFunc(before, isMarkerRune)
	if rest == before {
		return start
	}
	if marker := before[len(rest):]; marker == "." || marker == ")" {
		rest = strings.TrimRightFunc(rest, unicode.IsDigit)
	}
	if line := strings.TrimRightFunc(text[:floor+len(rest)], isHorizontalSpace); line != "" {
		if last, _ := utf8.DecodeLastRuneInString(line); !isLineBreak(last) {
			return start
		}
	}
	return floor + len(rest)
}

type speakerTurn struct {
	speaker string
	style   string
	text    string
}

// labelSpace matches every rune strings.Fields splits on, so a label agrees
// with the --multi-speaker name it was declared as ("Dr\u00a0Who:" is "Dr Who").
const labelSpace = `[\s\v\x{85}\p{Z}]`

// speakerTurns splits a script at each "<speaker>:" (or "<speaker> [style]:" /
// "<speaker> (style):") label that names one of speakers and strips the label
// from the turn text (Gemini 3.8 TTS treats text as a verbatim transcript and
// takes the speaker and style via speech_metadata). A label may sit at the
// start of a line (optionally after a list or quote marker: "-", "•", ">", "“",
// "1.") or mid-line after punctuation ("Alice: hi. Bob: hello.", "Alice:Bob:"),
// but not inside a longer word ("MaryAnn:") or after a word on the same line
// ("I told Bob: wait."). Labels for undeclared speakers are not split on, and
// turns with nothing to say are dropped. Every block must name a speaker, so
// text before the first label is an error.
func speakerTurns(text string, speakers []string) ([]speakerTurn, error) {
	// Longest first, so "Alice" wins over a declared "Al".
	sorted := slices.Clone(speakers)
	slices.SortFunc(sorted, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	names := make([]string, len(sorted))
	for i, speaker := range sorted {
		words := strings.Fields(speaker)
		for j, word := range words {
			words[j] = regexp.QuoteMeta(word)
		}
		names[i] = strings.Join(words, labelSpace+`+`)
	}
	styleMod := `(?:` + labelSpace + `*(?:\[([^\]\r\n]+)\]|\(([^\)\r\n]+)\)))?`
	label := regexp.MustCompile(`(` + strings.Join(names, "|") + `)` + styleMod + labelSpace + `*:`)
	hint := `Start each turn with its speaker label, e.g. "Alice: hi. Bob: hello."`

	// RE2 has no lookbehind: matches inside a longer word ("MaryAnn:") or after
	// a word on the same line ("I told Bob:") are skipped by hand, resuming one
	// rune later so they cannot hide a real label.
	type labelSpan struct{ start, nameStart, nameEnd, styleStart, styleEnd, end int }
	var labels []labelSpan
	for pos := 0; pos < len(text); {
		m := label.FindStringSubmatchIndex(text[pos:])
		if m == nil {
			break
		}
		start := pos + m[0]
		before := strings.TrimRightFunc(text[:start], isHorizontalSpace)
		if prev, _ := utf8.DecodeLastRuneInString(before); len(before) > 0 && isWordRune(prev) && (len(before) == start || len(labels) > 0) {
			_, size := utf8.DecodeRuneInString(text[start:])
			pos = start + size
			continue
		}
		floor := 0
		if len(labels) > 0 {
			floor = labels[len(labels)-1].end
		}
		styleStart, styleEnd := 0, 0
		if m[4] >= 0 {
			styleStart, styleEnd = pos+m[4], pos+m[5]
		} else if m[6] >= 0 {
			styleStart, styleEnd = pos+m[6], pos+m[7]
		}
		labels = append(labels, labelSpan{lineMarkerStart(text, floor, start), pos + m[2], pos + m[3], styleStart, styleEnd, pos + m[1]})
		pos += m[1]
	}
	if len(labels) == 0 {
		return nil, usageError("--multi-speaker: the text has no speaker label", hint,
			"Labels are case-sensitive and must match the --multi-speaker names")
	}
	if lead := strings.TrimSpace(text[:labels[0].start]); lead != "" {
		if runes := []rune(lead); len(runes) > 60 {
			lead = string(runes[:60]) + "…"
		}
		return nil, usageError(fmt.Sprintf("--multi-speaker: text before the first speaker label: %q", lead), hint)
	}
	var turns []speakerTurn
	for i, l := range labels {
		end := len(text)
		if i+1 < len(labels) {
			end = labels[i+1].start
		}
		turnText := strings.TrimSpace(text[l.end:end])
		if turnText == "" {
			continue
		}
		var turnStyle string
		if l.styleEnd > l.styleStart {
			turnStyle = strings.TrimSpace(text[l.styleStart:l.styleEnd])
		}
		turns = append(turns, speakerTurn{
			speaker: strings.Join(strings.Fields(text[l.nameStart:l.nameEnd]), " "),
			style:   turnStyle,
			text:    turnText,
		})
	}
	if len(turns) == 0 {
		return nil, usageError("--multi-speaker: every speaker turn is empty", hint)
	}
	return turns, nil
}

// buildSpeechConfig turns --voice / --multi-speaker / --language into the
// interaction's speech configuration: a single unnamed voice (array-of-one) or,
// for --multi-speaker, a per-speaker map. The returned label names the voice
// selection for the progress line.
func buildSpeechConfig(cmd *cobra.Command) (*interactions.SpeechConfigUnion, string, error) {
	var lang *string
	if l, _ := flagutil.GetStringFlag(cmd, "language"); strings.TrimSpace(l) != "" {
		lang = stringPtr(strings.TrimSpace(l))
	}
	spec, _ := flagutil.GetStringFlag(cmd, "multi-speaker")
	if strings.TrimSpace(spec) == "" {
		voice, _ := flagutil.GetStringFlag(cmd, "voice")
		voice = strings.TrimSpace(voice)
		if voice == "" {
			voice = defaultTTSVoice
		}
		u := interactions.CreateSpeechConfigUnionArrayOfSpeechConfig([]interactions.SpeechConfig{{
			Voice: stringPtr(voice), Language: lang,
		}})
		return &u, voice, nil
	}
	if flagutil.FlagChanged(cmd, "voice") {
		return nil, "", usageError("--voice and --multi-speaker are mutually exclusive")
	}
	var speakers []interactions.SpeechConfig
	seen := map[string]bool{}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, voice, ok := strings.Cut(pair, "=")
		// Names stay case-sensitive: each must match its label in the script.
		name, voice = strings.Join(strings.Fields(name), " "), strings.TrimSpace(voice)
		if !ok || name == "" || voice == "" {
			return nil, "", usageError(fmt.Sprintf("invalid --multi-speaker entry %q: expected Speaker=Voice", pair))
		}
		if seen[name] {
			return nil, "", usageError(fmt.Sprintf("--multi-speaker names speaker %q more than once", name))
		}
		seen[name] = true
		speakers = append(speakers, interactions.SpeechConfig{
			Speaker: stringPtr(name), Voice: stringPtr(voice), Language: lang,
		})
	}
	// The API takes exactly two speakers and rejects any other count opaquely.
	if len(speakers) != 2 {
		msg := fmt.Sprintf("--multi-speaker needs exactly two Speaker=Voice entries (got %d)", len(speakers))
		if len(speakers) == 1 {
			return nil, "", usageError(msg, "Use --voice for a single speaker")
		}
		return nil, "", usageError(msg)
	}
	u := interactions.CreateSpeechConfigUnionSpeakerConfig(interactions.SpeakerConfig{Speakers: speakers})
	return &u, "multi-speaker " + strings.TrimSpace(spec), nil
}

// extractAudio concatenates the inline audio blocks of the completed
// interaction's model-output steps and returns the decoded bytes plus the
// reported MIME type, channel count, and sample rate (0 when the API omits
// them). Raw PCM (audio/l16, …) is the common case for the TTS models. Blocks
// that disagree on format cannot be joined into one playable file.
func extractAudio(it *interactions.Interaction) ([]byte, string, int, int, error) {
	if err := interactionOutcome(it); err != nil {
		return nil, "", 0, 0, err
	}
	var audio []byte
	mimeType := ""
	channels, sampleRate := 0, 0
	for _, step := range it.Steps {
		if step.ModelOutputStep == nil {
			continue
		}
		for _, block := range step.ModelOutputStep.Content {
			ac := block.AudioContent
			if ac == nil || ac.Data == nil {
				continue
			}
			chunk, err := base64.StdEncoding.DecodeString(*ac.Data)
			if err != nil {
				return nil, "", 0, 0, runtimeError(fmt.Sprintf("audio payload is not valid base64: %v", err))
			}
			blockMIME, blockChannels, blockRate := "", 0, 0
			if ac.MimeType != nil {
				blockMIME = string(*ac.MimeType)
			}
			if ac.Channels != nil {
				blockChannels = *ac.Channels
			}
			if ac.SampleRate != nil {
				blockRate = *ac.SampleRate
			}
			if len(audio) == 0 {
				mimeType, channels, sampleRate = blockMIME, blockChannels, blockRate
			} else if blockMIME != mimeType || blockChannels != channels || blockRate != sampleRate {
				return nil, "", 0, 0, runtimeError("the API returned audio blocks in different formats, which cannot be joined into one file")
			}
			audio = append(audio, chunk...)
		}
	}
	if len(audio) == 0 {
		if msg := firstInteractionError(it); msg != "" {
			return nil, "", 0, 0, runtimeError("the API returned no audio: "+msg,
				"Check that the model supports speech output (default: "+defaultTTSModel+")")
		}
		return nil, "", 0, 0, runtimeError("no audio data in the API response",
			"Check that the model supports speech output (default: "+defaultTTSModel+")")
	}
	if mimeType == "" {
		// The TTS models return raw 16-bit PCM; assume L16 when the block omits
		// its MIME so the bytes are still wrapped as a playable WAV.
		mimeType = "audio/l16"
	}
	return audio, mimeType, channels, sampleRate, nil
}
