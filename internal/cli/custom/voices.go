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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/interactive"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/sdkerrors"
	"github.com/google-gemini/gemini-api-cli/internal/usage"
	"github.com/spf13/cobra"
)

var voiceIDShape = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// normalizeVoiceID strips an optional "voices/" prefix and validates that the
// resulting identifier is a single safe path segment.
func normalizeVoiceID(raw string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(raw), "voices/")
	if !voiceIDShape.MatchString(trimmed) || trimmed == "." || trimmed == ".." {
		return "", fmt.Errorf("invalid voice id %q; expected voices/<id> or a bare id like \"voice_abc123\"", strings.TrimSpace(raw))
	}
	return trimmed, nil
}

func newVoicesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "voices",
		Short:       "Design, list, inspect, and delete custom voices (" + defaultTTSModel + ")",
		Long:        "Design custom prompted voices from text descriptions, or list, inspect, and delete voices.\n\nUse \"gemini-api voices create <prompt>\" to design a reusable voice_<id> and optionally\nsave its sample preview WAV (--out), then pass \"--voice voice_<id>\" to \"gemini-api tts\".",
		GroupID:     "manage",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"speakeasy_cli_group": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if usageRequested(cmd) {
				return emitUsageKDL(cmd, cmd.OutOrStdout())
			}
			return cmd.Help()
		},
	}
	helpMeta(cmd, "model "+defaultTTSModel+" · stored voices expire after 48h unused",
		"https://ai.google.dev/gemini-api/docs/voice-design", "")

	for _, sub := range []*cobra.Command{
		newVoicesCreateCmd(),
		newVoicesListCmd(),
		newVoicesGetCmd(),
		newVoicesDeleteCmd(),
	} {
		usage.MarkDynamic(sub)
		cmd.AddCommand(sub)
	}
	usage.MarkDynamic(cmd)
	return cmd
}

func newVoicesCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create [prompt]",
		Short: "Design a custom voice from a text prompt (" + defaultTTSModel + ")",
		Long: "Design a custom voice from a natural-language prompt and store it for reuse with\n" +
			"\"gemini-api tts --voice <id>\". By default, stdout prints the created voice_<id>.\n" +
			"Pass --out to also save the generated sample preview WAV to disk.\n\n" +
			"Arguments:\n" +
			"  <prompt>  Voice description (or use --prompt, -f / --file, --stdin, or --body)",
		Example: "  gemini-api voices create \"A warm, thoughtful astronomer with a British accent\"\n" +
			"  gemini-api voices create \"Gravelly film-noir detective\" --base-voice Puck --out sample.wav\n" +
			"  gemini-api voices create \"Calm audiobook narrator\" --display-name \"Narrator\" --language en-US",
		Args: cobra.ArbitraryArgs,
		RunE: runVoicesCreate,
	}
	helpMeta(cmd, "model "+defaultTTSModel+" · type prompted · store true",
		"https://ai.google.dev/gemini-api/docs/voice-design",
		"pass --body for raw CreateVoiceRequest JSON (e.g. replicated voices)")

	cmd.Flags().String("prompt", "", "Voice description prompt (alternative to positional [prompt])")
	cmd.Flags().String("base-voice", "", "Existing prebuilt voice or custom voice_<id> to remix with the prompt")
	cmd.Flags().String("display-name", "", "Human-readable display name for the voice")
	cmd.Flags().String("language", "", "BCP-47 language code for the voice (e.g. en-US)")
	cmd.Flags().String("gender", "", "Voice gender metadata (e.g. female, male, neutral)")
	cmd.Flags().String("accent", "", "Voice accent metadata (e.g. british, southern-us)")
	cmd.Flags().String("pitch", "", "Voice pitch (low, medium, high)")
	cmd.Flags().StringP("file", "f", "", "Read the voice prompt from a file")
	cmd.Flags().Bool("stdin", false, "Read the voice prompt from stdin")
	cmd.Flags().String("out", "", "Write the returned sample_audio WAV preview to this file or directory")
	cmd.Flags().String("body", "", "Raw CreateVoiceRequest JSON object, @file, or @- (overrides prompted flags)")
	modelFlag(cmd, defaultTTSModel, "tts")

	// Secondary metadata flags kept hidden so `voices create --help` stays within the 40-line budget.
	cmd.Flags().String("description", "", "Description of the voice")
	cmd.Flags().String("persona", "", "Voice persona metadata")
	cmd.Flags().String("context", "", "Voice usage context metadata")
	cmd.Flags().String("region-code", "", "Region code for the voice (e.g. US, GB)")
	cmd.Flags().Bool("store", true, "Store the created voice server-side to receive a reusable voice_<id>")
	for _, hidden := range []string{"description", "persona", "context", "region-code", "store"} {
		_ = cmd.Flags().MarkHidden(hidden)
	}

	declareInteractive(cmd, interactive.CommandSpec{Args: []interactive.ArgSpec{{
		Name: "prompt", Summary: "Voice description prompt", Required: true, Variadic: true,
		SatisfiedBy: []string{"prompt", "file", "stdin", "body"},
	}}})
	return cmd
}

func resolveVoicePromptInput(cmd *cobra.Command, args []string) (string, error) {
	promptFlag, _ := flagutil.GetStringFlag(cmd, "prompt")
	promptFlag = strings.TrimSpace(promptFlag)
	if promptFlag != "" {
		if len(args) > 0 {
			return "", usageError("cannot combine positional prompt with --prompt")
		}
		if f, _ := flagutil.GetStringFlag(cmd, "file"); f != "" {
			return "", usageError("cannot combine --prompt with --file")
		}
		if s, _ := flagutil.GetBoolFlag(cmd, "stdin"); s {
			return "", usageError("cannot combine --prompt with --stdin")
		}
		return promptFlag, nil
	}
	return textInput(cmd, args, "file", "stdin")
}

func runVoicesCreate(cmd *cobra.Command, args []string) error {
	if usageRequested(cmd) {
		return emitUsageKDL(cmd, cmd.OutOrStdout())
	}

	var payloadBytes []byte
	bodyFlag, _ := flagutil.GetStringFlag(cmd, "body")
	if strings.TrimSpace(bodyFlag) != "" {
		if len(args) > 0 || flagutil.FlagChanged(cmd, "prompt") || flagutil.FlagChanged(cmd, "file") || flagutil.FlagChanged(cmd, "stdin") || flagutil.FlagChanged(cmd, "base-voice") {
			return usageError("--body cannot be combined with [prompt], --prompt, --file, --stdin, or --base-voice")
		}
		resolvedBody, err := flagutil.ResolveBodyFlagValue(cmd, "body", bodyFlag)
		if err != nil {
			return usageError(err.Error())
		}
		var bodyMap map[string]any
		if err := json.Unmarshal([]byte(resolvedBody), &bodyMap); err != nil || bodyMap == nil {
			return usageError("invalid --body: expected a JSON object")
		}
		payloadBytes, err = json.Marshal(bodyMap)
		if err != nil {
			return usageError(fmt.Sprintf("invalid --body JSON: %v", err))
		}
	} else {
		prompt, err := resolveVoicePromptInput(cmd, args)
		if err != nil {
			return err
		}
		if prompt == "" {
			return output.UsageHelpError(cmd, errors.New("missing required voice prompt (pass it as an argument, --prompt, --file, --stdin, or --body)"))
		}
		if flagutil.FlagChanged(cmd, "pitch") {
			if err := flagutil.ValidateEnumFlag(cmd, "pitch", []string{"low", "medium", "high"}); err != nil {
				return err
			}
		}
		model, err := resolveModel(cmd, defaultTTSModel)
		if err != nil {
			return err
		}
		if isLegacyMultiSpeakerModel(model) {
			return usageError(fmt.Sprintf("custom voice design is not supported by %s (requires %s)", model, defaultTTSModel))
		}
		promptedSpec := map[string]any{
			"input": prompt,
		}
		if baseVoice, _ := flagutil.GetStringFlag(cmd, "base-voice"); strings.TrimSpace(baseVoice) != "" {
			promptedSpec["base_voice"] = strings.TrimSpace(baseVoice)
		}
		voiceSpec := map[string]any{
			"type":     "prompted",
			"model":    model,
			"prompted": promptedSpec,
		}
		for flagName, jsonKey := range map[string]string{
			"display-name": "display_name",
			"description":  "description",
			"language":     "language_code",
			"region-code":  "region_code",
			"gender":       "gender",
			"accent":       "accent",
			"persona":      "persona",
			"context":      "context",
			"pitch":        "pitch",
		} {
			if val, _ := flagutil.GetStringFlag(cmd, flagName); strings.TrimSpace(val) != "" {
				voiceSpec[jsonKey] = strings.TrimSpace(val)
			}
		}
		store, _ := flagutil.GetBoolFlag(cmd, "store")
		reqMap := map[string]any{
			"store": store,
			"voice": voiceSpec,
		}
		payloadBytes, err = json.Marshal(reqMap)
		if err != nil {
			return err
		}
	}

	t, err := newRawTransport(cmd)
	if err != nil {
		return output.Error(cmd, err)
	}
	createURL := t.apiURL(url.PathEscape(t.apiVersion) + "/voices")
	req, err := t.newRequest(cmd.Context(), http.MethodPost, createURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	if isDryRun(cmd) {
		res, err := t.do(req)
		if res != nil && res.Body != nil {
			res.Body.Close()
		}
		return err
	}

	progress(cmd, "Designing custom voice...")
	res, err := t.do(req)
	if err != nil {
		return output.Error(cmd, err)
	}
	defer res.Body.Close()
	rawBody, err := io.ReadAll(res.Body)
	if err != nil {
		return runtimeError(fmt.Sprintf("reading voice response: %v", err))
	}

	var respMap map[string]any
	if err := json.Unmarshal(rawBody, &respMap); err != nil {
		return runtimeError(fmt.Sprintf("decoding voice response: %v", err))
	}

	voiceID, _ := respMap["id"].(string)
	if voiceID == "" {
		voiceID, _ = respMap["key"].(string)
	}
	if voiceID == "" {
		return runtimeError("the API response did not include a voice id or key")
	}
	progress(cmd, "Created voice %s.", voiceID)

	if outVal, _ := flagutil.GetStringFlag(cmd, "out"); strings.TrimSpace(outVal) != "" {
		samplePath, err := writeVoiceSampleAudio(cmd, respMap)
		if err != nil {
			return err
		}
		if samplePath != "" {
			respMap["sample_audio_path"] = samplePath
		}
	}
	return emitResult(cmd, voiceID, respMap)
}

// designVoiceForTTS creates a stored prompted voice via POST /{api_version}/voices
// and returns its voice_<id> so `gemini-api tts --voice-prompt` can synthesize
// immediately with the newly designed voice.
func designVoiceForTTS(cmd *cobra.Command, model, prompt, language string) (string, error) {
	voiceSpec := map[string]any{
		"type":  "prompted",
		"model": model,
		"prompted": map[string]any{
			"input": prompt,
		},
	}
	if language != "" {
		voiceSpec["language_code"] = language
	}
	payloadBytes, err := json.Marshal(map[string]any{
		"store": true,
		"voice": voiceSpec,
	})
	if err != nil {
		return "", err
	}
	t, err := newRawTransport(cmd)
	if err != nil {
		return "", output.Error(cmd, err)
	}
	createURL := t.apiURL(url.PathEscape(t.apiVersion) + "/voices")
	req, err := t.newRequest(cmd.Context(), http.MethodPost, createURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	if isDryRun(cmd) {
		res, err := t.do(req)
		if res != nil && res.Body != nil {
			res.Body.Close()
		}
		if err != nil {
			return "", err
		}
		return "voice_dry_run", nil
	}

	progress(cmd, "Designing custom voice with %s...", model)
	res, err := t.do(req)
	if err != nil {
		return "", output.Error(cmd, err)
	}
	defer res.Body.Close()
	rawBody, err := io.ReadAll(res.Body)
	if err != nil {
		return "", runtimeError(fmt.Sprintf("reading voice response: %v", err))
	}
	var respMap map[string]any
	if err := json.Unmarshal(rawBody, &respMap); err != nil {
		return "", runtimeError(fmt.Sprintf("decoding voice response: %v", err))
	}
	voiceID, _ := respMap["id"].(string)
	if voiceID == "" {
		return "", runtimeError("voice design succeeded but the API response had no voice id")
	}
	progress(cmd, "Designed custom voice %s (reuse with --voice %s).", voiceID, voiceID)
	return voiceID, nil
}

// writeVoiceSampleAudio decodes sample_audio from a Voice response and writes
// it as a playable file (wrapping raw PCM in a WAV header when needed).
func writeVoiceSampleAudio(cmd *cobra.Command, respMap map[string]any) (string, error) {
	sampleRaw, ok := respMap["sample_audio"].(map[string]any)
	if !ok || sampleRaw == nil {
		return "", runtimeError("the API response did not include sample_audio to write to --out")
	}
	b64Data, _ := sampleRaw["data"].(string)
	if b64Data == "" {
		return "", runtimeError("the API response sample_audio had no audio data")
	}
	audio, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		return "", runtimeError(fmt.Sprintf("sample_audio payload is not valid base64: %v", err))
	}
	mimeType, _ := sampleRaw["mime_type"].(string)
	if mimeType == "" {
		mimeType = "audio/wav"
	}
	format := parseAudioMIME(mimeType)
	data := audio
	if format.isPCM && !bytes.HasPrefix(audio, []byte("RIFF")) {
		channels := format.channels
		if channels == 0 {
			channels = 1
		}
		sampleRate := format.sampleRate
		if sampleRate == 0 {
			sampleRate = 24000
		}
		data = append(wavHeader(len(audio), sampleRate, channels), audio...)
	}
	ext, ok := extensionForAudioMIME(mimeType)
	if !ok {
		ext = ".wav"
	}
	path, err := artifactPath(cmd, "out", "gemini-voice", ext)
	if err != nil {
		return "", err
	}
	if err := writeArtifact(path, data); err != nil {
		return "", runtimeError(fmt.Sprintf("cannot write %s: %v", path, err))
	}
	progress(cmd, "Wrote sample preview %s (%d bytes).", path, len(data))
	return path, nil
}

var voicesPaginationProbe = output.PaginationProbe{
	Type:       "cursor",
	CursorKind: "string",
	NextCursor: "$.next_page_token",
}

type voiceResponseEnvelope struct {
	RawResponse *http.Response
	Result      any
	Next        func() (*voiceResponseEnvelope, error)
}

func newVoicesListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List prebuilt and custom voices",
		Long:  "List prebuilt and custom voices available to your project, with optional filtering by type, language, gender, accent, pitch, or search query.",
		Example: "  gemini-api voices list\n" +
			"  gemini-api voices list --type prompted\n" +
			"  gemini-api voices list --type prebuilt --language en-US --search warm",
		Args: cobra.NoArgs,
		Annotations: map[string]string{
			"speakeasy_operation": "ListVoices",
		},
		RunE: runVoicesList,
	}
	helpMeta(cmd, "returns both prebuilt and stored custom voices",
		"https://ai.google.dev/gemini-api/docs/voice-design", "")

	cmd.Flags().StringSlice("type", nil, "Filter by voice type: prompted, replicated, prebuilt")
	cmd.Flags().String("search", "", "Free-text search across voice names and descriptions")
	cmd.Flags().StringSlice("language", nil, "Filter by BCP-47 language code (e.g. en-US)")
	cmd.Flags().StringSlice("gender", nil, "Filter by gender (e.g. female, male)")
	cmd.Flags().StringSlice("accent", nil, "Filter by accent")
	cmd.Flags().StringSlice("pitch", nil, "Filter by pitch (low, medium, high)")
	cmd.Flags().Int("page-size", 0, "Maximum number of voices to return per page")
	cmd.Flags().String("page-token", "", "Page token from a previous voices list call")
	cmd.Flags().BoolP("all", "a", false, "Automatically paginate and fetch all results (streams NDJSON for JSON output)")
	cmd.Flags().Int("max-pages", 0, "Maximum number of pages to fetch when using --all (0 = no limit)")

	// Secondary filter flags kept hidden so `voices list --help` stays within the 40-line budget.
	cmd.Flags().StringSlice("persona", nil, "Filter by persona")
	cmd.Flags().StringSlice("context", nil, "Filter by usage context")
	cmd.Flags().StringSlice("region-code", nil, "Filter by region code")
	for _, hidden := range []string{"persona", "context", "region-code"} {
		_ = cmd.Flags().MarkHidden(hidden)
	}
	return cmd
}

func runVoicesList(cmd *cobra.Command, _ []string) error {
	if usageRequested(cmd) {
		return emitUsageKDL(cmd, cmd.OutOrStdout())
	}
	allPages, _ := flagutil.GetBoolFlag(cmd, "all")
	maxPages, _ := flagutil.GetIntFlag(cmd, "max-pages")
	if maxPages < 0 {
		return flagutil.WithCLIValidation(fmt.Errorf("--max-pages must be zero or greater"))
	}
	if flagutil.FlagChanged(cmd, "max-pages") && !allPages {
		return flagutil.WithCLIValidation(fmt.Errorf("--max-pages requires --all"))
	}

	t, err := newRawTransport(cmd)
	if err != nil {
		return output.Error(cmd, err)
	}
	pageToken, _ := flagutil.GetStringFlag(cmd, "page-token")

	if isDryRun(cmd) {
		req, err := buildVoicesListRequest(cmd, t, pageToken)
		if err != nil {
			return err
		}
		res, err := t.do(req)
		if res != nil && res.Body != nil {
			res.Body.Close()
		}
		return err
	}

	if allPages {
		res, err := fetchVoicesPage(cmd, t, pageToken)
		if err != nil {
			return output.Error(cmd, err)
		}
		return output.PaginatedResult(cmd, res, "Result", "", maxPages, voicesPaginationProbe)
	}

	stopProgress := output.StartRequestProgress(cmd)
	defer stopProgress()
	res, err := fetchVoicesPage(cmd, t, pageToken)
	stopProgress()
	if err != nil {
		return output.Error(cmd, err)
	}
	morePages := output.HasMorePages(res, voicesPaginationProbe)
	if err := output.Result(cmd, res); err != nil {
		return err
	}
	if morePages && !isDryRun(cmd) && !output.IsMachineMode(cmd) {
		fmt.Fprintln(cmd.ErrOrStderr(), "Hint: more pages available. Use --all to fetch all results, or --page-token for manual pagination.")
	}
	return nil
}

func buildVoicesListRequest(cmd *cobra.Command, t *rawTransport, pageToken string) (*http.Request, error) {
	q := url.Values{}
	for flagName, queryKey := range map[string]string{
		"type":        "type",
		"language":    "language_code",
		"region-code": "region_code",
		"accent":      "accent",
		"persona":     "persona",
		"context":     "context",
		"gender":      "gender",
		"pitch":       "pitch",
	} {
		vals, _ := cmd.Flags().GetStringSlice(flagName)
		for _, v := range vals {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				q.Add(queryKey, trimmed)
			}
		}
	}
	if search, _ := flagutil.GetStringFlag(cmd, "search"); strings.TrimSpace(search) != "" {
		q.Set("search", strings.TrimSpace(search))
	}
	if pageSize, _ := flagutil.GetIntFlag(cmd, "page-size"); flagutil.FlagChanged(cmd, "page-size") {
		q.Set("page_size", strconv.Itoa(pageSize))
	}
	if pageToken != "" {
		q.Set("page_token", pageToken)
	}
	listURL := t.apiURL(url.PathEscape(t.apiVersion) + "/voices")
	if encoded := q.Encode(); encoded != "" {
		listURL += "?" + encoded
	}
	return t.newRequest(cmd.Context(), http.MethodGet, listURL, nil)
}

func doVoicesRequestWithRetry(cmd *cobra.Command, t *rawTransport, makeReq func() (*http.Request, error)) (*http.Response, []byte, error) {
	noRetries := resolveNoRetries(cmd) || isDryRun(cmd)
	maxAttempts := 4
	if noRetries {
		maxAttempts = 1
	}
	backoff := 500 * time.Millisecond
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := makeReq()
		if err != nil {
			return nil, nil, err
		}
		res, err := t.do(req)
		if err != nil {
			var sdkErr *sdkerrors.SDKDefaultError
			if attempt < maxAttempts && errors.As(err, &sdkErr) && (sdkErr.StatusCode == http.StatusTooManyRequests || sdkErr.StatusCode == http.StatusServiceUnavailable) {
				wait := backoff
				if sdkErr.RawResponse != nil {
					if ra := sdkErr.RawResponse.Header.Get("Retry-After"); ra != "" {
						if secs, parseErr := strconv.Atoi(strings.TrimSpace(ra)); parseErr == nil && secs > 0 {
							wait = time.Duration(secs) * time.Second
						}
					}
				}
				select {
				case <-cmd.Context().Done():
					return nil, nil, cmd.Context().Err()
				case <-time.After(wait):
				}
				backoff = time.Duration(float64(backoff) * 1.5)
				continue
			}
			return nil, nil, err
		}
		rawBody, readErr := io.ReadAll(res.Body)
		res.Body.Close()
		if readErr != nil {
			return nil, nil, readErr
		}
		res.Body = io.NopCloser(bytes.NewReader(rawBody))
		return res, rawBody, nil
	}
	return nil, nil, fmt.Errorf("request failed after %d attempts", maxAttempts)
}

func fetchVoicesPage(cmd *cobra.Command, t *rawTransport, pageToken string) (*voiceResponseEnvelope, error) {
	res, rawBody, err := doVoicesRequestWithRetry(cmd, t, func() (*http.Request, error) {
		return buildVoicesListRequest(cmd, t, pageToken)
	})
	if err != nil {
		return nil, err
	}
	var parsed any
	if len(rawBody) > 0 {
		if err := json.Unmarshal(rawBody, &parsed); err != nil {
			parsed = string(rawBody)
		}
	}
	env := &voiceResponseEnvelope{
		RawResponse: res,
		Result:      parsed,
	}
	if pageMap, ok := parsed.(map[string]any); ok {
		if nextToken, _ := pageMap["next_page_token"].(string); strings.TrimSpace(nextToken) != "" {
			env.Next = func() (*voiceResponseEnvelope, error) {
				return fetchVoicesPage(cmd, t, nextToken)
			}
		}
	}
	return env, nil
}

func resolveVoiceIDArg(cmd *cobra.Command, args []string) (string, error) {
	flagVal, _ := flagutil.GetStringFlag(cmd, "id")
	if len(args) > 1 {
		return "", usageError(fmt.Sprintf("accepts at most 1 argument ([id]), received %d", len(args)))
	}
	if len(args) == 1 && flagutil.FlagChanged(cmd, "id") {
		return "", usageError("cannot pass both positional [id] and --id")
	}
	raw := flagVal
	if len(args) == 1 {
		raw = args[0]
	}
	if strings.TrimSpace(raw) == "" {
		if flagutil.FlagChanged(cmd, "id") || len(args) == 1 {
			return "", usageError("flag --id is blank; path parameters require a non-empty value")
		}
		return "", usageError("missing required flag: --id (or pass [id] as an argument)")
	}
	norm, err := normalizeVoiceID(raw)
	if err != nil {
		return "", usageError(err.Error())
	}
	return norm, nil
}

func newVoicesGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get [id]",
		Short: "Get a voice's metadata and sample preview",
		Long: "Get metadata for a stored custom voice or prebuilt voice by ID.\n" +
			"Pass --out to write the voice's sample_audio WAV preview to a local file.\n\n" +
			"Arguments:\n" +
			"  <id>  Voice ID, e.g. voice_abc123 or voices/voice_abc123 (or use --id)",
		Example: "  gemini-api voices get voice_abc123\n" +
			"  gemini-api voices get voice_abc123 --out preview.wav\n" +
			"  gemini-api voices get --id voice_abc123",
		Args: cobra.ArbitraryArgs,
		Annotations: map[string]string{
			"speakeasy_operation": "GetVoice",
		},
		RunE: runVoicesGet,
	}
	helpMeta(cmd, "", "https://ai.google.dev/gemini-api/docs/voice-design", "")
	cmd.Flags().StringP("id", "i", "", "Required. Voice ID to retrieve, as voices/<id> or a bare id (or pass [id])")
	cmd.Flags().String("out", "", "Write the voice's sample_audio WAV preview to this file or directory")
	declareInteractive(cmd, interactive.CommandSpec{Args: []interactive.ArgSpec{{
		Name: "id", Summary: "Voice ID", Required: true,
		SatisfiedBy: []string{"id"},
	}}})
	return cmd
}

func runVoicesGet(cmd *cobra.Command, args []string) error {
	if usageRequested(cmd) {
		return emitUsageKDL(cmd, cmd.OutOrStdout())
	}
	voiceID, err := resolveVoiceIDArg(cmd, args)
	if err != nil {
		return err
	}
	t, err := newRawTransport(cmd)
	if err != nil {
		return output.Error(cmd, err)
	}
	getURL := t.apiURL(url.PathEscape(t.apiVersion) + "/voices/" + url.PathEscape(voiceID))
	if isDryRun(cmd) {
		req, err := t.newRequest(cmd.Context(), http.MethodGet, getURL, nil)
		if err != nil {
			return err
		}
		res, err := t.do(req)
		if res != nil && res.Body != nil {
			res.Body.Close()
		}
		return err
	}

	stopProgress := output.StartRequestProgress(cmd)
	defer stopProgress()
	res, rawBody, err := doVoicesRequestWithRetry(cmd, t, func() (*http.Request, error) {
		return t.newRequest(cmd.Context(), http.MethodGet, getURL, nil)
	})
	stopProgress()
	if err != nil {
		return output.Error(cmd, err)
	}

	var respMap map[string]any
	if err := json.Unmarshal(rawBody, &respMap); err != nil {
		return runtimeError(fmt.Sprintf("decoding voice response: %v", err))
	}
	if outVal, _ := flagutil.GetStringFlag(cmd, "out"); strings.TrimSpace(outVal) != "" {
		samplePath, err := writeVoiceSampleAudio(cmd, respMap)
		if err != nil {
			return err
		}
		if samplePath != "" {
			respMap["sample_audio_path"] = samplePath
			if reencoded, err := json.Marshal(respMap); err == nil {
				res.Body = io.NopCloser(bytes.NewReader(reencoded))
			}
		}
	}
	return output.Result(cmd, &voiceResponseEnvelope{
		RawResponse: res,
		Result:      respMap,
	})
}

func newVoicesDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete [id]",
		Short: "Delete a stored custom voice by ID",
		Long: "Delete a stored custom voice by ID.\n\n" +
			"Arguments:\n" +
			"  <id>  Voice ID to delete, e.g. voice_abc123 or voices/voice_abc123 (or use --id)",
		Example: "  gemini-api voices delete voice_abc123\n" +
			"  gemini-api voices delete --id voices/voice_abc123",
		Args: cobra.ArbitraryArgs,
		Annotations: map[string]string{
			"speakeasy_operation": "DeleteVoice",
		},
		RunE: runVoicesDelete,
	}
	helpMeta(cmd, "", "https://ai.google.dev/gemini-api/docs/voice-design", "")
	cmd.Flags().StringP("id", "i", "", "Required. Voice ID to delete, as voices/<id> or a bare id (or pass [id])")
	declareInteractive(cmd, interactive.CommandSpec{Args: []interactive.ArgSpec{{
		Name: "id", Summary: "Voice ID", Required: true,
		SatisfiedBy: []string{"id"},
	}}})
	return cmd
}

func runVoicesDelete(cmd *cobra.Command, args []string) error {
	if usageRequested(cmd) {
		return emitUsageKDL(cmd, cmd.OutOrStdout())
	}
	voiceID, err := resolveVoiceIDArg(cmd, args)
	if err != nil {
		return err
	}
	t, err := newRawTransport(cmd)
	if err != nil {
		return output.Error(cmd, err)
	}
	delURL := t.apiURL(url.PathEscape(t.apiVersion) + "/voices/" + url.PathEscape(voiceID))
	req, err := t.newRequest(cmd.Context(), http.MethodDelete, delURL, nil)
	if err != nil {
		return err
	}
	if isDryRun(cmd) {
		res, err := t.do(req)
		if res != nil && res.Body != nil {
			res.Body.Close()
		}
		return err
	}

	stopProgress := output.StartRequestProgress(cmd)
	defer stopProgress()
	res, err := t.do(req)
	stopProgress()
	if err != nil {
		return output.Error(cmd, err)
	}
	rawBody, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		return runtimeError(fmt.Sprintf("reading delete response: %v", err))
	}
	res.Body = io.NopCloser(bytes.NewReader(rawBody))
	var parsed any
	if len(rawBody) > 0 {
		_ = json.Unmarshal(rawBody, &parsed)
	}
	return output.Result(cmd, &voiceResponseEnvelope{
		RawResponse: res,
		Result:      parsed,
	})
}
