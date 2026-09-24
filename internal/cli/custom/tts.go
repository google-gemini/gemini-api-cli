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
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

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
	cmd.Long = "Convert text to speech.\n\nGenerated audio is written to a local file as WAV (24 kHz mono). The extension\nfollows the audio: --out foo.mp3 is written as foo.wav, and --out naming a\ndirectory places a default-named file inside it. An existing file is replaced.\nBy default, stdout prints the absolute artifact path. The request is not stored\nserver-side.\n\nArguments:\n  <text>  Text to speak (or use -f / --stdin)"
	cmd.Example = "  gemini-api tts \"Welcome to the show\"                    # → ./gemini-tts-<unix-ms>-<random>.wav\n" +
		"  gemini-api tts -f script.txt --voice Puck --out out.wav\n" +
		"  echo \"hello\" | gemini-api tts --stdin\n" +
		"  gemini-api tts \"Alice: hi. Bob: hello.\" --multi-speaker \"Alice=Kore,Bob=Puck\""
	helpMeta(cmd, "model "+defaultTTSModel+" · voice "+defaultTTSVoice+" · 24kHz mono WAV",
		"https://ai.google.dev/gemini-api/docs/speech-generation",
		"full request control via gemini-api agent run")
	cmd.Args = cobra.ArbitraryArgs
	cmd.Flags().StringP("voice", "v", defaultTTSVoice, "Voice for single-speaker audio (e.g. Kore, Puck, Zephyr, Charon, Fenrir, Aoede)")
	cmd.Flags().String("multi-speaker", "", "Speaker→voice map for two-speaker scripts, e.g. \"Alice=Kore,Bob=Puck\"")
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
	speech, voiceLabel, err := buildSpeechConfig(cmd)
	if err != nil {
		return err
	}
	// The format stays bare: the TTS models reject every explicit mime_type
	// and delivery ("not supported for models/…"), and answer with 24 kHz L16.
	responseFormat := interactions.CreateCreateModelInteractionResponseFormatResponseFormat(
		interactions.CreateResponseFormatAudioResponseFormat(interactions.AudioResponseFormat{}))
	// Warnings reach a human on stderr and a machine in the result envelope;
	// structured stderr stays reserved for the error document.
	var warnings []string
	if speech.SpeakerConfig != nil {
		// The API does not check names against the script; a stray one is
		// silently voiced wrong.
		for _, sp := range speech.SpeakerConfig.Speakers {
			if !containsWord(text, *sp.Speaker) {
				warnings = append(warnings, fmt.Sprintf("speaker %q does not appear in the text", *sp.Speaker))
			}
		}
	}
	for _, warning := range warnings {
		progress(cmd, "warning: %s", warning)
	}
	body := newModelInteraction(model, textContentBlock(text))
	body.ResponseFormat = &responseFormat
	body.GenerationConfig = &interactions.GenerationConfig{SpeechConfig: speech}
	req := operations.CreateInteractionRequest{
		Body: operations.CreateCreateInteractionRequestBodyCreateModelInteraction(body),
	}

	s, err := client.NewClient(cmd)
	if err != nil {
		return err
	}
	opts, err := callOpts(cmd)
	if err != nil {
		return err
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
	edge := `[^\p{L}\p{N}]`
	return regexp.MustCompile(`(^|` + edge + `)` + regexp.QuoteMeta(word) + `($|` + edge + `)`).MatchString(text)
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
