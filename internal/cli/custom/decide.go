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
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google-gemini/gemini-api-cli/internal/client"
	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/interactive"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/decisions"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/genai"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/interactions"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/operations"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/sdkerrors"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/retry"
	"github.com/google-gemini/gemini-api-cli/internal/sdk"
	"github.com/google-gemini/gemini-api-cli/internal/usage"
	"github.com/spf13/cobra"
)

const (
	decideDocsURL          = "https://aistudio.google.com/docs/decisions-api"
	decideDefaultMaxRetry  = 4
	decideDefaultInitialMs = 500
	decideDefaultMaxMs     = 8000
	decideDefaultExponent  = 2.0
	decideDefaultBudgetMs  = 30000
)

// attachDecide turns the claimed decide command into Decisions API porcelain.
func attachDecide(cmd *cobra.Command) {
	cmd.Use = "decide [question]"
	cmd.Short = "Ask yes/no, pick-one, and rating questions about text or media (" + defaultDecisionModel + ")"
	cmd.Long = "Evaluate yes/no (noul), pick-one (choice), or rating (score) questions against\ntext or media and return calibrated probabilities.\n\nPass local files, files/<id>, or https:// links to images, audio, video, or PDFs\nwith -i, or pipe text with --stdin. Pass a quoted question for one yes/no check\nunder questions.q, or --body for choice, score, and batch questions (--schema\nshows the request schema). Local files share this CLI's 20 MB inline budget."
	cmd.Example = "  gemini-api decide -i doorstep.jpg \"Is a package clearly visible?\"\n" +
		"  gemini-api decide -i ticket.txt --body '{\"questions\":{\"route\":{\"type\":\"choice\",\"instructions\":\"Which queue?\",\"criteria\":{\"billing\":\"Payments\",\"technical\":\"Bugs\",\"other\":\"Anything else\"}}}}'\n" +
		"  gemini-api decide -i ticket.txt --body @route.json"
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["speakeasy_operation"] = "CreateDecision"
	cmd.Annotations[flagutil.AnnotationWholeBodyFlag] = "body"
	helpMeta(cmd, "model "+defaultDecisionModel, decideDocsURL, "")

	cmd.Args = cobra.ArbitraryArgs
	cmd.Flags().StringArrayP("input", "i", nil, "Local path, files/<id>, or https:// image/audio/video/PDF link (repeatable)")
	cmd.Flags().String("mime-type", "", "Override the detected MIME type (one --input only)")
	cmd.Flags().Bool("stdin", false, "Read input text from stdin to EOF instead of a JSON body")
	cmd.Flags().String("body", "", "Request body as JSON (@path reads a file, @- reads stdin; see --schema)")
	cmd.Flags().Bool("schema", false, "Print the exact JSON Schema of the request body and exit")
	cmd.Flags().StringP("model", "m", "", "Override model (default: "+defaultDecisionModel+")")

	flagutil.MarkRequestInput(cmd, "input", "mime-type", "stdin", "model")
	annotatePromptFlag(cmd, "input", flagutil.PromptFlagSpec{
		Required:    true,
		Kind:        "string-array",
		Order:       0,
		BodySources: []string{"body", "stdin"},
	})
	annotatePromptFlag(cmd, "body", flagutil.PromptFlagSpec{Kind: "json", BodyFlag: true})
	annotatePromptFlag(cmd, "schema", flagutil.PromptFlagSpec{Kind: "bool", DocSurface: true})
	declareInteractive(cmd, interactive.CommandSpec{Args: []interactive.ArgSpec{{
		Name:        "question",
		Summary:     "Yes/no question to evaluate (stored under questions.q)",
		Required:    true,
		Variadic:    true,
		BodyKey:     "questions",
		SatisfiedBy: []string{"body"},
	}}})
	cmd.RunE = runDecide
}

func runDecide(cmd *cobra.Command, args []string) error {
	if usageRequested(cmd) {
		return emitUsageKDL(cmd, cmd.OutOrStdout())
	}
	if requested, _ := cmd.Flags().GetBool("schema"); requested {
		return usage.EmitBodySchema(cmd.OutOrStdout(), "CreateDecision")
	}

	inputs, err := cmd.Flags().GetStringArray("input")
	if err != nil {
		return output.CLIError(cmd, err)
	}
	mimeOverride, _ := flagutil.GetStringFlag(cmd, "mime-type")
	mimeOverride = strings.TrimSpace(mimeOverride)
	if mimeOverride != "" {
		if len(inputs) == 0 {
			return usageError("--mime-type requires --input")
		}
		if len(inputs) > 1 {
			return usageError("--mime-type can only be used with exactly one --input")
		}
	}

	stdinTextFlag, _ := flagutil.GetBoolFlag(cmd, "stdin")
	bodyChanged := flagutil.FlagChanged(cmd, "body")
	if stdinTextFlag && bodyChanged {
		if v, _ := flagutil.GetStringFlag(cmd, "body"); strings.TrimSpace(v) == "@-" {
			return usageError("--stdin and --body @- both read stdin; pass the body from a file (@path) or inline instead")
		}
	}

	var stdinText string
	var inlineBytes int64
	if stdinTextFlag {
		// Bound memory read to maxInlineBytes+1 so oversized pipes fail fast.
		raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxInlineBytes+1))
		if err != nil {
			return usageError(fmt.Sprintf("cannot read stdin: %v", err))
		}
		if int64(len(raw)) > maxInlineBytes {
			return usageError(fmt.Sprintf("stdin text exceeds this CLI's %d MB inline text limit", maxInlineBytes>>20))
		}
		if !utf8.Valid(raw) {
			return usageError("stdin is not valid UTF-8 text")
		}
		text := strings.TrimPrefix(string(raw), "\uFEFF")
		if strings.TrimSpace(text) == "" {
			return usageError("stdin is empty")
		}
		stdinText = ensureTrailingNewline(text)
		inlineBytes += int64(len(stdinText))
		if inlineBytes > maxInlineBytes {
			return usageError(fmt.Sprintf("stdin text is %d bytes; this CLI sends text inline, up to %d MB per request", len(stdinText), maxInlineBytes>>20))
		}
	}

	var (
		bodyMap    map[string]any
		bodySource string
		hasBody    bool
	)
	if bodyChanged {
		if err := flagutil.ResolveBodyFlag(cmd, "body"); err != nil {
			return output.CLIError(cmd, err)
		}
		rawStr, _ := flagutil.GetStringFlag(cmd, "body")
		bodySource = "--body"
		hasBody = true
		if err := decodeDecideBodyJSON([]byte(rawStr), bodySource, &bodyMap); err != nil {
			return err
		}
	} else if !stdinTextFlag && !(len(inputs) > 0 && len(args) > 0) {
		// Loop safety: only read piped stdin as a JSON body when neither --body
		// nor --stdin was given, and the command line does not already carry
		// both -i and a positional question.
		data, err := flagutil.ReadStdinBody(cmd, "body")
		if err != nil {
			return output.CLIError(cmd, err)
		}
		if len(bytes.TrimSpace(data)) > 0 {
			bodySource = "stdin"
			hasBody = true
			if err := decodeDecideBodyJSON(data, bodySource, &bodyMap); err != nil {
				return err
			}
		}
	}

	// Bare invocation with no inputs, no question, no body, and no request flags
	// gets the full command help on stderr (matching other tier-1 commands).
	if len(inputs) == 0 && !stdinTextFlag && len(args) == 0 && !hasBody &&
		!flagutil.FlagChanged(cmd, "model") && mimeOverride == "" {
		return output.UsageHelpError(cmd, errors.New("missing input and question (pass -i <input> \"<question>\" or --body <json>)"))
	}

	// Validate positional question and body questions completely before any
	// keychain access or network calls.
	hasPositional := len(args) > 0
	positionalQuestion := strings.TrimSpace(strings.Join(args, " "))
	if hasPositional && positionalQuestion == "" {
		return usageError("question cannot be empty")
	}

	if bodyMap == nil {
		bodyMap = map[string]any{}
	}
	rawBodyQuestions, hasBodyQuestionsKey := bodyMap["questions"]
	hasBodyQuestions := hasBodyQuestionsKey && rawBodyQuestions != nil
	if hasPositional && hasBodyQuestions {
		return usageError(fmt.Sprintf("cannot combine a positional question with \"questions\" in %s", bodySource))
	}
	if hasPositional {
		bodyMap["questions"] = map[string]any{
			"q": map[string]any{
				"type":         "noul",
				"instructions": positionalQuestion,
			},
		}
	} else {
		if err := validateBodyQuestions(rawBodyQuestions, hasBodyQuestionsKey); err != nil {
			return err
		}
	}

	// Resolve model: reject duplicate --model + body "model"; default to gemini-decision-1.
	if err := resolveDecideModel(cmd, bodyMap, bodySource); err != nil {
		return err
	}

	// Prevalidate all -i inputs (local files, web URLs, Files API refs) and body
	// input shape before creating the client or making any network calls.
	parsedInputs, hasNonBlankCLIInput, err := prevalidateDecideInputs(cmd, inputs, mimeOverride, inlineBytes)
	if err != nil {
		return err
	}
	if stdinText != "" {
		hasNonBlankCLIInput = true
	}
	hasNonBlankBodyInput, err := prevalidateDecideBodyInput(cmd, bodyMap, len(parsedInputs) > 0 || stdinText != "")
	if err != nil {
		return err
	}
	if !hasNonBlankCLIInput && !hasNonBlankBodyInput {
		return usageError("input cannot be empty")
	}

	// Parse retry flags before making any network calls so invalid flag values
	// fail locally with exit 2.
	retryPolicy, err := resolveDecideRetryPolicy(cmd)
	if err != nil {
		return err
	}

	// Build SDK client (validates --server-url, --timeout, --header, etc.).
	s, err := client.NewClient(cmd)
	if err != nil {
		return err
	}

	// Resolve -i blocks in order, then --stdin text block, then body input.
	var cliBlocks []any
	var dryRunBodyDelta int64
	if len(parsedInputs) > 0 {
		cliBlocks = make([]any, 0, len(parsedInputs)+1)
		runningInline := inlineBytes
		for _, item := range parsedInputs {
			blk, addedBytes, delta, err := item.buildBlock(cmd, s)
			if err != nil {
				return err
			}
			dryRunBodyDelta += delta
			if runningInline += addedBytes; runningInline > maxInlineBytes {
				return usageError(fmt.Sprintf("%s: inline inputs grew past this CLI's %d MB request budget while the command ran", item.ref, maxInlineRequestBytes>>20))
			}
			cliBlocks = append(cliBlocks, blk)
		}
	}
	if stdinText != "" {
		cliBlocks = append(cliBlocks, map[string]any{"type": "text", "text": stdinText})
	}

	if existingInput, ok := bodyMap["input"]; ok && existingInput != nil {
		if err := normalizeDecideBodyInputURIs(cmd, s, existingInput); err != nil {
			return err
		}
	}

	if len(cliBlocks) > 0 {
		merged, err := mergeDecideInputs(cliBlocks, bodyMap["input"])
		if err != nil {
			return err
		}
		bodyMap["input"] = merged
	}
	if err := requireNonEmptyDecideInput(bodyMap["input"]); err != nil {
		return err
	}

	// Marshal merged JSON into the typed SDK request so wire serialization and
	// dry-run previews match the OpenAPI schema exactly, and enforce the 20 MB
	// inline request budget across the entire request body (including --body).
	encoded, err := json.Marshal(bodyMap)
	if err != nil {
		return output.CLIError(cmd, err)
	}
	if totalSize := int64(len(encoded)) + dryRunBodyDelta; totalSize > maxInlineRequestBytes {
		return usageError(
			fmt.Sprintf("request body is %d bytes, which exceeds this CLI's %d MB request budget; this CLI keeps inline requests under %d MB", totalSize, maxInlineRequestBytes>>20, maxInlineRequestBytes>>20),
			"Upload large media with \"gemini-api files upload <path>\" and pass the returned files/<id>",
		)
	}
	var reqBody decisions.CreateDecisionRequest
	if err := json.Unmarshal(encoded, &reqBody); err != nil {
		return usageError(fmt.Sprintf("invalid request body: %v", err), "Run \"gemini-api decide --schema\" to inspect the request JSON schema")
	}

	req := operations.CreateDecisionRequest{Body: reqBody}
	opts, err := output.PrepareCallOpts(cmd)
	if err != nil {
		return err
	}
	rawJSON := output.WantsRawJSON(cmd)
	if isDryRun(cmd) || rawJSON {
		opts = append(opts, operations.WithSkipDeserialization())
	}

	if isDryRun(cmd) {
		res, err := s.Decisions.Create(cmd.Context(), req, opts...)
		if res != nil && res.HTTPMeta.Response != nil && res.HTTPMeta.Response.Body != nil {
			_ = res.HTTPMeta.Response.Body.Close()
		}
		if err != nil {
			return output.Error(cmd, err)
		}
		return nil
	}

	stopProgress := output.StartRequestProgress(cmd)
	defer stopProgress()
	res, err := callDecideWithRetry(cmd.Context(), s, req, opts, retryPolicy, rawJSON)
	stopProgress()
	if err != nil {
		return output.Error(cmd, err)
	}
	return output.Result(cmd, res)
}

func decodeDecideBodyJSON(raw []byte, source string, dst *map[string]any) error {
	var parsed map[string]any
	if err := decodeBodyJSON(raw, &parsed); err != nil {
		if source == "stdin" {
			return usageError(fmt.Sprintf("failed to parse stdin as JSON: %v", err),
				"Piped stdin is read as the JSON request body; add --stdin to send it as input text instead")
		}
		return usageError(fmt.Sprintf("failed to parse %s as JSON: %v", source, err))
	}
	if parsed == nil {
		return usageError(fmt.Sprintf("%s must be a JSON object", source))
	}
	*dst = parsed
	return nil
}

func validateBodyQuestions(raw any, present bool) error {
	if !present || raw == nil {
		return usageError("missing question: pass a yes/no question as an argument or provide \"questions\" in --body")
	}
	qMap, ok := raw.(map[string]any)
	if !ok {
		return usageError("\"questions\" in request body must be a JSON object keyed by question ID")
	}
	if len(qMap) == 0 {
		return usageError("\"questions\" in request body must contain at least one question")
	}
	for id, qRaw := range qMap {
		if strings.TrimSpace(id) == "" {
			return usageError("question ID in \"questions\" cannot be empty")
		}
		qObj, ok := qRaw.(map[string]any)
		if !ok || qObj == nil {
			return usageError(fmt.Sprintf("question %q must be a JSON object", id))
		}
		instr, hasInstr := qObj["instructions"]
		if !hasInstr || instr == nil {
			if _, hasSingular := qObj["instruction"]; hasSingular {
				return usageError(fmt.Sprintf("question %q is missing \"instructions\" (note: the field name is plural \"instructions\", not \"instruction\")", id))
			}
			return usageError(fmt.Sprintf("question %q is missing required \"instructions\"", id))
		}
		if err := validateDecisionInstructions(id, instr); err != nil {
			return err
		}
	}
	// Validate the questions structure against the SDK types up front so schema
	// mistakes (e.g. unknown question type or wrong criteria shape) fail with
	// exit 2 before any Files API lookups run.
	encoded, err := json.Marshal(qMap)
	if err != nil {
		return usageError(fmt.Sprintf("invalid \"questions\" in request body: %v", err))
	}
	var typed map[string]decisions.Question
	if err := json.Unmarshal(encoded, &typed); err != nil {
		return usageError(fmt.Sprintf("invalid \"questions\" in request body: %v", err), "Run \"gemini-api decide --schema\" to inspect the request JSON schema")
	}
	return nil
}

func validateDecisionInstructions(questionID string, raw any) error {
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return usageError(fmt.Sprintf("question %q has blank instructions", questionID))
		}
		return nil
	case map[string]any:
		return validateInstructionBlock(questionID, v, true)
	case []any:
		if len(v) == 0 {
			return usageError(fmt.Sprintf("question %q has empty instructions", questionID))
		}
		hasNonBlank := false
		for _, item := range v {
			blk, ok := item.(map[string]any)
			if !ok || blk == nil {
				return usageError(fmt.Sprintf("question %q instructions elements must be text content block objects", questionID))
			}
			if err := validateInstructionBlock(questionID, blk, false); err != nil {
				return err
			}
			if text, _ := blk["text"].(string); strings.TrimSpace(text) != "" {
				hasNonBlank = true
			}
		}
		if !hasNonBlank {
			return usageError(fmt.Sprintf("question %q has blank instructions", questionID))
		}
		return nil
	default:
		return usageError(fmt.Sprintf("question %q \"instructions\" must be a string, a text content block, or a list of text content blocks", questionID))
	}
}

func validateInstructionBlock(questionID string, blk map[string]any, requireNonBlank bool) error {
	if objectType(blk) != "text" {
		return usageError(fmt.Sprintf("question %q instruction blocks must have \"type\": \"text\"", questionID))
	}
	text, ok := blk["text"].(string)
	if !ok {
		return usageError(fmt.Sprintf("question %q instruction text block must carry a string \"text\" field", questionID))
	}
	if requireNonBlank && strings.TrimSpace(text) == "" {
		return usageError(fmt.Sprintf("question %q has blank instructions", questionID))
	}
	return nil
}

func resolveDecideModel(cmd *cobra.Command, bodyMap map[string]any, bodySource string) error {
	existing, hasExisting := bodyMap["model"]
	if flagutil.FlagChanged(cmd, "model") {
		if hasExisting && existing != nil {
			return usageError(fmt.Sprintf("key \"model\" is set both by --model and by %s; pass exactly one", bodySource))
		}
		rawModel, _ := flagutil.GetStringFlag(cmd, "model")
		id, err := normalizeModelPositional(rawModel)
		if err != nil {
			return usageError("--model: " + err.Error())
		}
		bodyMap["model"] = id
		return nil
	}
	if hasExisting && existing != nil {
		s, ok := existing.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return usageError("\"model\" in request body must be a non-empty string")
		}
		return nil
	}
	bodyMap["model"] = defaultDecisionModel
	return nil
}

func ensureTrailingNewline(s string) string {
	if s != "" && !strings.HasSuffix(s, "\n") {
		return s + "\n"
	}
	return s
}

// decideContentClassOf classifies MIME types according to the Decisions API:
// images, audio, video, and PDF go in their typed blocks; all text/* (including
// CSV) and text-based application/* formats go in text blocks.
func decideContentClassOf(mimeType string) (class contentClass, sendable bool) {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		mt := interactions.ImageContentMimeType(mimeType)
		return contentImage, mt.IsExact()
	case strings.HasPrefix(mimeType, "audio/"):
		mt := interactions.AudioContentMimeType(mimeType)
		return contentAudio, mt.IsExact()
	case strings.HasPrefix(mimeType, "video/"):
		mt := interactions.VideoContentMimeType(mimeType)
		return contentVideo, mt.IsExact()
	case mimeType == "application/pdf":
		return contentDocument, true
	case strings.HasPrefix(mimeType, "text/") || textApplicationMIME[mimeType]:
		return contentText, true
	default:
		return contentDocument, false
	}
}

func decideBlockTypeForMIME(mimeType string) string {
	class, _ := decideContentClassOf(mimeType)
	switch class {
	case contentImage:
		return "image"
	case contentAudio:
		return "audio"
	case contentVideo:
		return "video"
	case contentText:
		return "text"
	default:
		return "document"
	}
}

// checkDecideURIMIME validates MIME types for URI-backed Decisions inputs
// (uploaded Files API resources and https:// web links). Text cannot be linked
// because text blocks only carry inline text; PDF is the only document type.
func checkDecideURIMIME(mimeType, ref string) error {
	if mimeType == "" {
		return nil
	}
	class, sendable := decideContentClassOf(mimeType)
	if class == contentText {
		return usageError(
			fmt.Sprintf("%s: text content (%s) cannot be referenced by link", ref, mimeType),
			"Pass it as a local file (-i <path>) or pipe it with --stdin",
		)
	}
	if !sendable {
		return usageError(
			fmt.Sprintf("%s: MIME type %s is not supported by Decisions links", ref, mimeType),
			"Supported linked inputs: images, audio, video, and PDF (application/pdf)",
		)
	}
	return nil
}

func decideMediaBlockMap(mimeType, payload string, inline bool) map[string]any {
	block := map[string]any{}
	if inline {
		block["data"] = payload
	} else {
		block["uri"] = payload
	}
	blockType := decideBlockTypeForMIME(mimeType)
	block["type"] = blockType
	if mimeType != "" && blockType != "text" {
		block["mime_type"] = mimeType
	}
	return block
}

type decideInputKind int

const (
	decideInputLocal decideInputKind = iota
	decideInputRemoteFile
	decideInputWebURL
)

type decideParsedInput struct {
	kind       decideInputKind
	ref        string
	arg        string
	mimeType   string
	fileName   string // canonical "files/<id>" for remote files
	fileID     string // bare "<id>" for remote files
	inlineCost int64
}

func isYouTubeURLCaseInsensitive(arg string) bool {
	lower := strings.ToLower(arg)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return false
	}
	u, err := url.Parse(arg)
	if err != nil {
		return false
	}
	return youtubeHost.MatchString(strings.ToLower(u.Hostname()))
}

func prevalidateDecideInputs(cmd *cobra.Command, inputs []string, mimeOverride string, existingInlineBytes int64) ([]decideParsedInput, bool, error) {
	if len(inputs) == 0 {
		return nil, false, nil
	}
	parsed := make([]decideParsedInput, 0, len(inputs))
	inlineBytes := existingInlineBytes
	hasNonBlank := false
	canonOverride := ""
	if mimeOverride != "" {
		canonOverride = canonicalRemoteMIME(mimeOverride)
	}

	for i, raw := range inputs {
		arg := strings.TrimSpace(raw)
		ref := inputRef(i+1, arg)
		if arg == "" {
			return nil, false, usageError(ref + ": input cannot be empty")
		}
		lower := strings.ToLower(arg)

		// 1. Refuse gs:// links locally, pointing to files register.
		if strings.HasPrefix(lower, "gs://") {
			return nil, false, usageError(
				ref+": gs:// links are not directly supported by decide",
				fmt.Sprintf("Register the object with \"gemini-api files register --uris '[%q]'\" and pass the returned files/<id>", arg),
			)
		}

		// 2. Refuse YouTube URLs locally.
		if isYouTubeURLCaseInsensitive(arg) {
			return nil, false, usageError(
				ref+": YouTube links are not supported by decide",
				"Download the media locally or upload it with \"gemini-api files upload <path>\" and pass files/<id>",
			)
		}

		// 3. Recognize full Files API URLs before general https:// / http:// handling.
		if fileRef, ok := extractFullFilesURI(cmd, arg); ok {
			name, id, valid := normalizeFileID(fileRef)
			if !valid {
				return nil, false, usageError(ref + ": invalid Files API reference; expected files/<id>")
			}
			if err := checkDecideURIMIME(canonOverride, ref); err != nil {
				return nil, false, err
			}
			hasNonBlank = true
			parsed = append(parsed, decideParsedInput{
				kind:     decideInputRemoteFile,
				ref:      ref,
				arg:      arg,
				mimeType: canonOverride,
				fileName: name,
				fileID:   id,
			})
			continue
		}

		// 4. Refuse plain http:// links locally.
		if strings.HasPrefix(lower, "http://") {
			return nil, false, usageError(
				ref+": plain http:// links are not supported; only https:// links are accepted",
				"Use an https:// URL, download the file locally, or upload it with \"gemini-api files upload <path>\"",
			)
		}

		// 5. Handle https:// web links for image, audio, video, and PDF.
		if strings.HasPrefix(lower, "https://") {
			u, err := url.Parse(arg)
			if err != nil || u.Hostname() == "" {
				return nil, false, usageError(ref + ": invalid https:// URL")
			}
			mt := canonOverride
			if mt == "" {
				ext := strings.ToLower(path.Ext(u.Path))
				if ext == "" {
					return nil, false, usageError(
						ref+": cannot determine MIME type from URL path (no file extension)",
						"Pass --mime-type explicitly (for example --mime-type application/pdf)",
					)
				}
				mt = canonicalRemoteMIME(detectMIME("file"+ext, ""))
				if mt == "" {
					return nil, false, usageError(
						fmt.Sprintf("%s: cannot determine MIME type for URL extension %q", ref, ext),
						"Pass --mime-type explicitly (for example --mime-type application/pdf)",
					)
				}
			}
			if err := checkDecideURIMIME(mt, ref); err != nil {
				return nil, false, err
			}
			hasNonBlank = true
			parsed = append(parsed, decideParsedInput{
				kind:     decideInputWebURL,
				ref:      ref,
				arg:      arg,
				mimeType: mt,
			})
			continue
		}

		// 6. Existing local path wins even if spelled like files/<name>.
		info, statErr := os.Stat(arg)
		if statErr == nil {
			cost, mt, err := checkDecideLocalFile(arg, info, canonOverride, ref)
			if err != nil {
				return nil, false, err
			}
			nonBlank, err := probeDecideLocalFile(arg, mt, ref)
			if err != nil {
				return nil, false, err
			}
			if nonBlank {
				hasNonBlank = true
			}
			inlineBytes += cost
			if inlineBytes > maxInlineBytes {
				return nil, false, usageError(
					fmt.Sprintf("%s: cumulative inline inputs (base64 included) exceed this CLI's %d MB request budget", ref, maxInlineRequestBytes>>20),
					fmt.Sprintf("Run \"gemini-api files upload %s\" and pass the returned files/<id> instead", arg),
				)
			}
			parsed = append(parsed, decideParsedInput{
				kind:       decideInputLocal,
				ref:        ref,
				arg:        arg,
				mimeType:   mt,
				inlineCost: cost,
			})
			continue
		}
		if errors.Is(statErr, fs.ErrPermission) {
			return nil, false, usageError(fmt.Sprintf("%s: cannot access file: %v", ref, statErr))
		}

		// 7. Short Files API reference: files/<id>.
		if !strings.HasPrefix(arg, "files/") {
			return nil, false, usageError(ref + ": file not found; for an uploaded file pass files/<id>")
		}
		name, id, valid := normalizeFileID(arg)
		if !valid {
			return nil, false, usageError(ref + ": invalid Files API reference; expected files/<id>")
		}
		if err := checkDecideURIMIME(canonOverride, ref); err != nil {
			return nil, false, err
		}
		hasNonBlank = true
		parsed = append(parsed, decideParsedInput{
			kind:     decideInputRemoteFile,
			ref:      ref,
			arg:      arg,
			mimeType: canonOverride,
			fileName: name,
			fileID:   id,
		})
	}
	return parsed, hasNonBlank, nil
}

func checkDecideLocalFile(pathStr string, info os.FileInfo, canonMimeOverride, ref string) (int64, string, error) {
	if !info.Mode().IsRegular() {
		return 0, "", usageError(ref + ": not a regular file")
	}
	if info.Size() == 0 {
		return 0, "", usageError(ref + ": file is empty")
	}
	mimeType := canonicalRemoteMIME(detectMIME(pathStr, canonMimeOverride))
	if mimeType == "" {
		return 0, "", usageError(ref+": cannot determine the MIME type",
			"Pass --mime-type explicitly (for example --mime-type image/png)")
	}
	class, sendable := decideContentClassOf(mimeType)
	if !sendable {
		return 0, "", usageError(fmt.Sprintf("%s: MIME type %s is not accepted by Decisions", ref, mimeType),
			"Supported local inputs: images, audio, video, PDF, and text files (including CSV, JSON, and code); convert the file or pass --mime-type if detection is wrong")
	}
	cost := inlineCost(info.Size(), class)
	if class == contentText {
		// Reserve 1 extra byte in case ensureTrailingNewline appends '\n'.
		cost++
		if cost > maxInlineBytes {
			return 0, "", usageError(fmt.Sprintf("%s: text file is %d bytes; this CLI sends text inline, up to %d MB per request", ref, info.Size(), maxInlineBytes>>20))
		}
	} else if cost > maxInlineBytes {
		return 0, "", usageError(
			fmt.Sprintf("%s: file is %d bytes (%d as base64); this CLI keeps inline requests under %d MB", ref, info.Size(), cost, maxInlineRequestBytes>>20),
			fmt.Sprintf("Run \"gemini-api files upload %s\" and pass the returned files/<id> instead", pathStr),
		)
	}
	return cost, mimeType, nil
}

func probeDecideLocalFile(pathStr, mimeType, ref string) (bool, error) {
	if class, _ := decideContentClassOf(mimeType); class == contentText {
		data, err := os.ReadFile(pathStr)
		if err != nil {
			return false, usageError(fmt.Sprintf("%s: cannot read file: %v", ref, err))
		}
		if !utf8.Valid(data) {
			return false, usageError(fmt.Sprintf("%s: detected as %s but the content is not UTF-8 text", ref, mimeType),
				"Pass --mime-type with the file's real media type")
		}
		text := strings.TrimPrefix(string(data), "\uFEFF")
		return strings.TrimSpace(text) != "", nil
	}
	f, err := os.Open(pathStr)
	if err != nil {
		return false, usageError(fmt.Sprintf("%s: cannot read file: %v", ref, err))
	}
	return true, f.Close()
}

func (item decideParsedInput) buildBlock(cmd *cobra.Command, s *sdk.GeminiAPI) (map[string]any, int64, int64, error) {
	switch item.kind {
	case decideInputWebURL:
		return decideMediaBlockMap(item.mimeType, item.arg, false), 0, 0, nil
	case decideInputRemoteFile:
		blk, err := resolveDecideRemoteFile(cmd, s, item.fileName, item.fileID, item.mimeType, item.ref)
		return blk, 0, 0, err
	default:
		return buildDecideLocalBlock(cmd, item)
	}
}

func buildDecideLocalBlock(cmd *cobra.Command, item decideParsedInput) (map[string]any, int64, int64, error) {
	class, _ := decideContentClassOf(item.mimeType)
	if isDryRun(cmd) {
		var size int64
		var realPayloadJSONLen int64
		if class == contentText {
			raw, err := os.ReadFile(item.arg)
			if err != nil {
				return nil, 0, 0, usageError(fmt.Sprintf("%s: cannot read file: %v", item.ref, err))
			}
			text := ensureTrailingNewline(strings.TrimPrefix(string(raw), "\uFEFF"))
			size = int64(len(raw))
			realJSON, _ := json.Marshal(text)
			realPayloadJSONLen = int64(len(realJSON))
		} else {
			info, err := os.Stat(item.arg)
			if err != nil {
				return nil, 0, 0, usageError(fmt.Sprintf("%s: cannot read file: %v", item.ref, err))
			}
			size = info.Size()
			realPayloadJSONLen = 2 + int64(base64.StdEncoding.EncodedLen(int(size)))
		}
		cost := inlineCost(size, class)
		if cost > maxInlineBytes {
			return nil, 0, 0, usageError(fmt.Sprintf("%s: file grew past the inline limit while the command ran", item.ref))
		}
		placeholder := fmt.Sprintf("<bytes:%d>", size)
		previewJSON, _ := json.Marshal(placeholder)
		delta := realPayloadJSONLen - int64(len(previewJSON))
		if class == contentText {
			return map[string]any{"type": "text", "text": placeholder}, cost, delta, nil
		}
		return decideMediaBlockMap(item.mimeType, placeholder, true), cost, delta, nil
	}

	raw, err := os.ReadFile(item.arg)
	if err != nil {
		return nil, 0, 0, usageError(fmt.Sprintf("%s: cannot read file: %v", item.ref, err))
	}
	if class == contentText {
		if !utf8.Valid(raw) {
			return nil, 0, 0, usageError(fmt.Sprintf("%s: detected as %s but the content is not UTF-8 text", item.ref, item.mimeType),
				"Pass --mime-type with the file's real media type")
		}
		text := ensureTrailingNewline(strings.TrimPrefix(string(raw), "\uFEFF"))
		cost := int64(len(text))
		if cost > maxInlineBytes {
			return nil, 0, 0, usageError(fmt.Sprintf("%s: file grew past the inline limit while the command ran", item.ref))
		}
		return map[string]any{"type": "text", "text": text}, cost, 0, nil
	}
	cost := inlineCost(int64(len(raw)), class)
	if cost > maxInlineBytes {
		return nil, 0, 0, usageError(fmt.Sprintf("%s: file grew past the inline limit while the command ran", item.ref))
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	return decideMediaBlockMap(item.mimeType, encoded, true), cost, 0, nil
}

// resolveDecideRemoteFile looks up a Files API resource for Decisions.
// Under --dry-run, no lookup request is sent or previewed: a placeholder block
// with the expanded Files API URI is built locally.
func resolveDecideRemoteFile(cmd *cobra.Command, s *sdk.GeminiAPI, name, id, mimeOverride, ref string) (map[string]any, error) {
	fileURI := expandedFilesURI(cmd, name)
	mimeType := canonicalRemoteMIME(mimeOverride)
	if err := checkDecideURIMIME(mimeType, ref); err != nil {
		return nil, err
	}
	if isDryRun(cmd) {
		return decideMediaBlockMap(mimeType, fileURI, false), nil
	}
	opts, err := callOpts(cmd)
	if err != nil {
		return nil, err
	}
	res, err := s.Files.FilesGet(cmd.Context(), operations.FilesGetRequest{File: id}, opts...)
	if err != nil {
		return nil, output.Error(cmd, err)
	}
	if res.File == nil {
		return nil, runtimeError(fmt.Sprintf("%s: the Files API returned no metadata", ref))
	}
	if res.File.URI != nil && *res.File.URI != "" {
		fileURI = *res.File.URI
	}
	if mimeType == "" && res.File.MimeType != nil {
		mimeType = canonicalRemoteMIME(*res.File.MimeType)
	}
	state := genai.FileState("")
	if res.File.State != nil {
		state = *res.File.State
	}
	switch state {
	case genai.FileStateActive:
	case genai.FileStateProcessing:
		return nil, runtimeError(fmt.Sprintf("%s: %s is still processing", ref, name),
			fmt.Sprintf("Wait for it to become ACTIVE: gemini-api files get %s", name))
	case genai.FileStateFailed:
		return nil, runtimeError(fmt.Sprintf("%s: %s failed processing on the server", ref, name))
	default:
		if state == "" {
			state = "unset"
		}
		return nil, runtimeError(fmt.Sprintf("%s: %s is not ACTIVE (state: %s)", ref, name, state),
			fmt.Sprintf("Check it: gemini-api files get %s", name))
	}
	if err := checkDecideURIMIME(mimeType, ref); err != nil {
		return nil, err
	}
	return decideMediaBlockMap(mimeType, fileURI, false), nil
}

// prevalidateDecideBodyInput checks body["input"] shape and embedded file/link
// refs locally before any keychain access or network calls.
func prevalidateDecideBodyInput(cmd *cobra.Command, bodyMap map[string]any, hasCLIInput bool) (bool, error) {
	raw, exists := bodyMap["input"]
	if !exists || raw == nil {
		if !hasCLIInput {
			return false, usageError("missing input: pass -i <file|files/id|https-url>, --stdin, or set \"input\" in --body")
		}
		return false, nil
	}
	switch v := raw.(type) {
	case string:
		if !hasCLIInput && strings.TrimSpace(v) == "" {
			return false, usageError("input cannot be empty")
		}
		return strings.TrimSpace(v) != "", nil
	case map[string]any:
		return prevalidateDecideBodyBlock(cmd, v)
	case []any:
		if len(v) == 0 && !hasCLIInput {
			return false, usageError("missing input: \"input\" in request body is empty")
		}
		hasNonBlank := false
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok || m == nil {
				return false, usageError("elements of \"input\" in request body must be content block objects")
			}
			nonBlank, err := prevalidateDecideBodyBlock(cmd, m)
			if err != nil {
				return false, err
			}
			if nonBlank {
				hasNonBlank = true
			}
		}
		return hasNonBlank, nil
	default:
		return false, usageError("\"input\" in request body must be a string, a content block object, or a list of content blocks")
	}
}

func prevalidateDecideBodyBlock(cmd *cobra.Command, obj map[string]any) (bool, error) {
	typ := objectType(obj)
	if typ != "" && !isContentBlockType(typ) {
		return false, usageError(fmt.Sprintf("unsupported input block type %q; Decisions expects flat content blocks (text, image, audio, video, document)", typ))
	}
	rawURI, _ := obj["uri"].(string)
	trimmed := strings.TrimSpace(rawURI)
	if trimmed != "" {
		ref := fmt.Sprintf("uri %q", rawURI)
		if typ == "text" {
			return false, usageError(
				ref+": text blocks cannot carry a uri",
				"Pass text inline in the block's \"text\" field, as a local file (-i <path>), or with --stdin",
			)
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "gs://") {
			return false, usageError(
				ref+": gs:// links are not directly supported by decide",
				fmt.Sprintf("Register the object with \"gemini-api files register --uris '[%q]'\" and pass the returned files/<id>", trimmed),
			)
		}
		if isYouTubeURLCaseInsensitive(trimmed) {
			return false, usageError(
				ref+": YouTube links are not supported by decide",
				"Download the media locally or upload it with \"gemini-api files upload <path>\" and pass files/<id>",
			)
		}
		if strings.HasPrefix(trimmed, "files/") || strings.HasPrefix(trimmed, "/files/") {
			if _, _, valid := normalizeFileID(strings.TrimPrefix(trimmed, "/")); !valid {
				return false, usageError(fmt.Sprintf("invalid Files API reference %q; expected files/<id>", rawURI))
			}
		} else if fileRef, matched := extractFullFilesURI(cmd, trimmed); matched {
			if _, _, valid := normalizeFileID(fileRef); !valid {
				return false, usageError(fmt.Sprintf("invalid Files API reference %q; expected files/<id>", rawURI))
			}
		} else if strings.HasPrefix(lower, "http://") {
			return false, usageError(
				ref+": plain http:// links are not supported; only https:// links are accepted",
				"Use an https:// URL, download the file locally, or upload it with \"gemini-api files upload <path>\"",
			)
		}
		if mt, _ := obj["mime_type"].(string); strings.TrimSpace(mt) != "" {
			canon := canonicalRemoteMIME(mt)
			if err := checkDecideURIMIME(canon, ref); err != nil {
				return false, err
			}
			if typ != "" && decideBlockTypeForMIME(canon) != typ {
				return false, usageError(fmt.Sprintf("%s: block type %q does not match mime_type %s (expected %q)", ref, typ, canon, decideBlockTypeForMIME(canon)))
			}
		}
	}
	if typ == "text" {
		t, _ := obj["text"].(string)
		return strings.TrimSpace(t) != "", nil
	}
	return true, nil
}

func normalizeDecideBodyInputURIs(cmd *cobra.Command, s *sdk.GeminiAPI, val any) error {
	switch v := val.(type) {
	case map[string]any:
		return normalizeDecideContentURI(cmd, s, v)
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if err := normalizeDecideContentURI(cmd, s, m); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func normalizeDecideContentURI(cmd *cobra.Command, s *sdk.GeminiAPI, obj map[string]any) error {
	rawURI, ok := obj["uri"].(string)
	if !ok || strings.TrimSpace(rawURI) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(rawURI)
	var (
		name    string
		id      string
		valid   bool
		rewrite bool
	)
	if strings.HasPrefix(trimmed, "files/") || strings.HasPrefix(trimmed, "/files/") {
		rewrite = true
		name, id, valid = normalizeFileID(strings.TrimPrefix(trimmed, "/"))
		if !valid {
			return usageError(fmt.Sprintf("invalid Files API reference %q; expected files/<id>", rawURI))
		}
	} else if fileRef, matched := extractFullFilesURI(cmd, trimmed); matched {
		rewrite = !fullFilesURIPattern.MatchString(trimmed)
		name, id, valid = normalizeFileID(fileRef)
		if !valid {
			return usageError(fmt.Sprintf("invalid Files API reference %q; expected files/<id>", rawURI))
		}
	} else {
		return nil
	}

	ref := fmt.Sprintf("uri %q", rawURI)
	existingMime, _ := obj["mime_type"].(string)
	existingMime = strings.TrimSpace(existingMime)
	existingType := objectType(obj)

	if existingMime == "" {
		resolved, err := resolveDecideRemoteFile(cmd, s, name, id, "", ref)
		if err != nil {
			return err
		}
		if mt, ok := resolved["mime_type"].(string); ok && mt != "" {
			resolvedType, _ := resolved["type"].(string)
			if existingType != "" && resolvedType != "" && existingType != resolvedType {
				return usageError(fmt.Sprintf("%s: block type %q does not match resolved MIME type %s (expected %q)", ref, existingType, mt, resolvedType))
			}
			obj["mime_type"] = mt
		}
		if u, ok := resolved["uri"].(string); ok && u != "" {
			obj["uri"] = u
		} else if rewrite {
			obj["uri"] = expandedFilesURI(cmd, name)
		}
		if existingType == "" {
			if t, ok := resolved["type"].(string); ok && t != "" {
				obj["type"] = t
			}
		}
		return nil
	}

	canonMime := canonicalRemoteMIME(existingMime)
	if err := checkDecideURIMIME(canonMime, ref); err != nil {
		return err
	}
	expectedType := decideBlockTypeForMIME(canonMime)
	if existingType != "" && existingType != expectedType {
		return usageError(fmt.Sprintf("%s: block type %q does not match mime_type %s (expected %q)", ref, existingType, canonMime, expectedType))
	}
	obj["mime_type"] = canonMime
	if rewrite {
		obj["uri"] = expandedFilesURI(cmd, name)
	}
	if existingType == "" {
		obj["type"] = expectedType
	}
	return nil
}

// mergeDecideInputs merges CLI-supplied input blocks ahead of any body input
// into a single flat content block list, leaving caller-authored body blocks
// unmodified.
func mergeDecideInputs(cliBlocks []any, existing any) ([]any, error) {
	if existing == nil {
		return append([]any(nil), cliBlocks...), nil
	}
	switch v := existing.(type) {
	case string:
		out := make([]any, 0, len(cliBlocks)+1)
		out = append(out, cliBlocks...)
		if v != "" {
			out = append(out, map[string]any{"type": "text", "text": v})
		}
		return out, nil
	case map[string]any:
		out := make([]any, 0, len(cliBlocks)+1)
		out = append(out, cliBlocks...)
		out = append(out, v)
		return out, nil
	case []any:
		out := make([]any, 0, len(cliBlocks)+len(v))
		out = append(out, cliBlocks...)
		out = append(out, v...)
		return out, nil
	default:
		return nil, usageError("\"input\" in request body must be a string, a content block object, or a list of content blocks")
	}
}

func requireNonEmptyDecideInput(input any) error {
	switch v := input.(type) {
	case nil:
		return usageError("missing input: pass -i <file|files/id|https-url>, --stdin, or set \"input\" in --body")
	case string:
		if strings.TrimSpace(v) == "" {
			return usageError("input cannot be empty")
		}
		return nil
	case map[string]any:
		if objectType(v) == "text" {
			if t, _ := v["text"].(string); strings.TrimSpace(t) == "" {
				return usageError("input cannot be empty")
			}
		}
		return nil
	case []any:
		if len(v) == 0 {
			return usageError("missing input: pass -i <file|files/id|https-url>, --stdin, or set \"input\" in --body")
		}
		hasNonBlank := false
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if objectType(m) != "text" {
				hasNonBlank = true
				break
			}
			if t, _ := m["text"].(string); strings.TrimSpace(t) != "" {
				hasNonBlank = true
				break
			}
		}
		if !hasNonBlank {
			return usageError("input cannot be empty")
		}
		return nil
	default:
		return nil
	}
}

type decideRetryPolicy struct {
	enabled               bool
	maxRetries            int
	initialInterval       time.Duration
	maxInterval           time.Duration
	exponent              float64
	maxElapsedTime        time.Duration
	retryConnectionErrors bool
}

func defaultDecideRetryPolicy() decideRetryPolicy {
	return decideRetryPolicy{
		enabled:               true,
		maxRetries:            decideDefaultMaxRetry,
		initialInterval:       decideDefaultInitialMs * time.Millisecond,
		maxInterval:           decideDefaultMaxMs * time.Millisecond,
		exponent:              decideDefaultExponent,
		maxElapsedTime:        decideDefaultBudgetMs * time.Millisecond,
		retryConnectionErrors: true,
	}
}

func resolveDecideRetryPolicy(cmd *cobra.Command) (decideRetryPolicy, error) {
	policy := defaultDecideRetryPolicy()
	if isDryRun(cmd) {
		policy.enabled = false
		return policy, nil
	}
	if noRetries, changed := flagutil.GetBoolFlag(cmd, "no-retries"); changed {
		if noRetries {
			policy.enabled = false
			return policy, nil
		}
	} else if config.GetString("no-retries") == "true" {
		policy.enabled = false
		return policy, nil
	}

	configJSON := ""
	if val, changed := flagutil.GetStringFlag(cmd, "retry-config"); changed && val != "" {
		configJSON = val
	} else if val := config.GetString("retry-config"); val != "" {
		configJSON = val
	}
	if configJSON != "" {
		cfg := retry.Config{RetryConnectionErrors: true}
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return policy, flagutil.WithCLIValidation(fmt.Errorf("invalid --retry-config JSON: %w", err))
		}
		switch cfg.Strategy {
		case "", "none":
			policy.enabled = false
			return policy, nil
		case "backoff", "attempt-count-backoff":
		default:
			return policy, flagutil.WithCLIValidation(fmt.Errorf("invalid --retry-config strategy %q (supported: backoff, attempt-count-backoff, none)", cfg.Strategy))
		}
		if cfg.MaxRetries != nil {
			policy.maxRetries = max(*cfg.MaxRetries, 0)
		}
		if cfg.Backoff != nil {
			if cfg.Backoff.InitialInterval > 0 {
				policy.initialInterval = time.Duration(cfg.Backoff.InitialInterval) * time.Millisecond
			}
			if cfg.Backoff.MaxInterval > 0 {
				policy.maxInterval = time.Duration(cfg.Backoff.MaxInterval) * time.Millisecond
			}
			if cfg.Backoff.Exponent > 0 {
				policy.exponent = cfg.Backoff.Exponent
			}
			if cfg.Backoff.MaxElapsedTime > 0 {
				policy.maxElapsedTime = time.Duration(cfg.Backoff.MaxElapsedTime) * time.Millisecond
			}
		}
		policy.retryConnectionErrors = cfg.RetryConnectionErrors
		return policy, nil
	}

	maxElapsedStr := ""
	if val, changed := flagutil.GetStringFlag(cmd, "retry-max-elapsed-time"); changed && val != "" {
		maxElapsedStr = val
	} else if val := config.GetString("retry-max-elapsed-time"); val != "" {
		maxElapsedStr = val
	}
	if maxElapsedStr != "" {
		dur, err := time.ParseDuration(maxElapsedStr)
		if err != nil {
			return policy, flagutil.WithCLIValidation(fmt.Errorf("invalid --retry-max-elapsed-time value %q: %w", maxElapsedStr, err))
		}
		if dur <= 0 {
			policy.enabled = false
			return policy, nil
		}
		policy.maxElapsedTime = dur
	}
	if retryConn, changed := flagutil.GetBoolFlag(cmd, "retry-connection-errors"); changed {
		policy.retryConnectionErrors = retryConn
	} else if val := strings.ToLower(strings.TrimSpace(config.GetString("retry-connection-errors"))); val == "true" || val == "false" {
		policy.retryConnectionErrors = val == "true"
	}
	return policy, nil
}

func callDecideWithRetry(
	ctx context.Context,
	s *sdk.GeminiAPI,
	req operations.CreateDecisionRequest,
	opts []operations.Option,
	policy decideRetryPolicy,
	bufferRawBody bool,
) (*operations.CreateDecisionResponse, error) {
	start := time.Now()
	for attempt := 0; ; attempt++ {
		res, err := s.Decisions.Create(ctx, req, opts...)
		if err == nil && bufferRawBody && res != nil && res.HTTPMeta.Response != nil && res.HTTPMeta.Response.Body != nil {
			// Buffer raw body inside the retry loop so mid-body disconnects under
			// -o json / --jq retry the same way as deserialized formats.
			raw, readErr := io.ReadAll(res.HTTPMeta.Response.Body)
			_ = res.HTTPMeta.Response.Body.Close()
			if readErr != nil {
				err = fmt.Errorf("error reading response body: %w", readErr)
				res = nil
			} else {
				res.HTTPMeta.Response.Body = io.NopCloser(bytes.NewReader(raw))
			}
		}
		if err == nil {
			return res, nil
		}
		if !policy.enabled || attempt >= policy.maxRetries {
			return nil, err
		}
		retryable, retryAfter := classifyDecideRetry(ctx, err, policy.retryConnectionErrors)
		if !retryable || ctx.Err() != nil {
			return nil, err
		}
		remaining := policy.maxElapsedTime - time.Since(start)
		if remaining <= 0 {
			return nil, err
		}
		var wait time.Duration
		if retryAfter > 0 {
			if retryAfter > remaining {
				return nil, err
			}
			wait = retryAfter
		} else {
			wait = decideBackoffDuration(policy, attempt)
			if wait > remaining {
				return nil, err
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}
	}
}

func classifyDecideRetry(ctx context.Context, err error, retryConn bool) (retryable bool, retryAfter time.Duration) {
	if err == nil || ctx.Err() != nil {
		return false, 0
	}
	var refused *client.RedirectRefusedError
	if errors.As(err, &refused) {
		return false, 0
	}
	var sdkErr *sdkerrors.SDKDefaultError
	if errors.As(err, &sdkErr) {
		code := sdkErr.StatusCode
		if code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || (code >= 500 && code <= 599) {
			return true, parseRetryAfter(sdkErr.RawResponse)
		}
		return false, 0
	}
	if !retryConn {
		return false, 0
	}
	if errors.Is(err, context.Canceled) {
		return false, 0
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true, 0
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && (urlErr.Timeout() || urlErr.Temporary()) {
		return true, 0
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true, 0
	}
	// Match HTTP/2 transport connection drops and server stream resets.
	msg := err.Error()
	if strings.Contains(msg, "http2: client connection lost") ||
		strings.Contains(msg, "http2: server sent GOAWAY") ||
		strings.Contains(msg, "stream error:") {
		return true, 0
	}
	return false, 0
}

const (
	maxRetryAfterSeconds = math.MaxInt64 / int64(time.Second)
	maxRetryAfterMillis  = math.MaxInt64 / int64(time.Millisecond)
)

func parseRetryAfter(res *http.Response) time.Duration {
	if res == nil {
		return 0
	}
	if rawMs := strings.TrimSpace(res.Header.Get("Retry-After-Ms")); rawMs != "" {
		if ms, err := strconv.ParseInt(rawMs, 10, 64); err == nil && ms > 0 {
			if ms >= maxRetryAfterMillis {
				return time.Duration(math.MaxInt64)
			}
			return time.Duration(ms) * time.Millisecond
		}
	}
	raw := strings.TrimSpace(res.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		if secs >= maxRetryAfterSeconds {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		if delta := time.Until(t); delta > 0 {
			return delta
		}
		return 0
	}
	if t, err := time.Parse(time.RFC1123Z, raw); err == nil {
		if delta := time.Until(t); delta > 0 {
			return delta
		}
		return 0
	}
	return 0
}

func decideBackoffDuration(policy decideRetryPolicy, attempt int) time.Duration {
	base := float64(policy.initialInterval) * math.Pow(policy.exponent, float64(attempt))
	// Full jitter down by up to 25% to avoid synchronized retries.
	base *= 1 - (rand.Float64() * 0.25)
	if base <= 0 {
		base = float64(policy.initialInterval)
	}
	if policy.maxInterval > 0 && base > float64(policy.maxInterval) {
		base = float64(policy.maxInterval)
	}
	return time.Duration(base)
}
