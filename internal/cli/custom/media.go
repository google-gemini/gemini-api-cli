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
	"io/fs"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google-gemini/gemini-api-cli/internal/client"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/genai"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/interactions"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/operations"
	"github.com/google-gemini/gemini-api-cli/internal/sdk"
	"github.com/spf13/cobra"
)

// maxInlineRequestBytes is the CLI's conservative budget for one interaction
// request that inlines local files; inlineRequestReserve is the share kept for
// the prompt, response schema, and JSON framing. Inputs are charged at their
// encoded size (see inlineCost). Larger inputs go through the Files API
// ("gemini-api files upload") and are referenced by files/<id>.
const (
	maxInlineRequestBytes = 20 << 20
	inlineRequestReserve  = 1 << 20
	maxInlineBytes        = maxInlineRequestBytes - inlineRequestReserve
)

// inlineCost is what a local file of the given size adds to the request body:
// text travels as is, everything else as base64 (4/3 of the raw size).
func inlineCost(size int64, class contentClass) int64 {
	if class == contentText {
		return size
	}
	return int64(base64.StdEncoding.EncodedLen(int(size)))
}

// inlineLimitNote is the help sentence analyze and transcribe share.
var inlineLimitNote = fmt.Sprintf("Local files are sent inline; this CLI keeps each request under %d MB, base64\nincluded (about %d MB of media). Upload larger files with \"gemini-api files upload\"\nand pass the returned files/<id>.",
	maxInlineRequestBytes>>20, (maxInlineBytes/4*3)>>20)

// mimeByExtension maps common media/document extensions to the MIME types the
// Interactions API accepts. It is consulted before the platform's mime database
// so results are stable across machines. Media entries use the spellings of the
// interactions mime_type enums (video/mov, not video/quicktime); every entry
// must classify as sendable (see contentClassOf), which the unit tests pin.
var mimeByExtension = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp",
	".gif": "image/gif", ".heic": "image/heic", ".heif": "image/heif", ".bmp": "image/bmp",
	".tif": "image/tiff", ".tiff": "image/tiff",
	".mp3": "audio/mp3", ".wav": "audio/wav", ".ogg": "audio/ogg", ".m4a": "audio/m4a",
	".flac": "audio/flac", ".aac": "audio/aac", ".opus": "audio/opus", ".aiff": "audio/aiff",
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/mov", ".avi": "video/avi",
	".mpeg": "video/mpeg", ".mpg": "video/mpeg", ".flv": "video/x-flv",
	".wmv": "video/wmv", ".3gp": "video/3gpp",
	".pdf": "application/pdf", ".csv": "text/csv",
	".txt": "text/plain", ".md": "text/markdown",
	".html": "text/html", ".htm": "text/html", ".xml": "text/xml", ".json": "application/json",
	".js": "text/javascript", ".ts": "text/x-typescript",
	".py": "text/x-python", ".go": "text/x-go", ".css": "text/css", ".yaml": "text/yaml", ".yml": "text/yaml",
}

// uploadOnlyMIME holds extensions the Files API stores but no interactions
// content block accepts: "files upload" still detects them, while analyze and
// transcribe reject them by name instead of as an unknown type.
var uploadOnlyMIME = map[string]string{
	".amr": "audio/amr", ".wma": "audio/x-ms-wma", ".mkv": "video/x-matroska", ".rtf": "application/rtf",
}

// detectMIME returns the lower-cased MIME type for a local path: explicit
// override first, then the curated tables, then the platform database. Empty
// when unknown.
func detectMIME(path, override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.ToLower(strings.TrimSpace(override))
	}
	ext := strings.ToLower(filepath.Ext(path))
	if m, ok := mimeByExtension[ext]; ok {
		return m
	}
	if m, ok := uploadOnlyMIME[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		// Drop parameters such as "; charset=utf-8" — the API wants the bare type.
		if i := strings.Index(m, ";"); i >= 0 {
			m = strings.TrimSpace(m[:i])
		}
		return strings.ToLower(m)
	}
	return ""
}

// contentClass is the interactions content block a MIME type travels in.
type contentClass int

const (
	contentDocument contentClass = iota
	contentImage
	contentAudio
	contentVideo
	contentText
)

// textApplicationMIME lists the application/* types that are plain text and so
// travel as a text block alongside text/*.
var textApplicationMIME = map[string]bool{
	"application/json": true, "application/xml": true, "application/yaml": true,
	"application/x-yaml": true, "application/javascript": true,
}

// contentClassOf picks the content block for a MIME type and reports whether
// the API accepts that exact type there. The image, audio, video, and document
// blocks each publish a closed mime_type enum (documents are only PDF and CSV);
// every other textual type is sent as a text block, which carries no MIME type.
// Anything else falls to the document block with sendable=false.
func contentClassOf(mimeType string) (class contentClass, sendable bool) {
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
	}
	if mt := interactions.DocumentContentMimeType(mimeType); mt.IsExact() {
		return contentDocument, true
	}
	if strings.HasPrefix(mimeType, "text/") || textApplicationMIME[mimeType] {
		return contentText, true
	}
	return contentDocument, false
}

// mediaPolicy is what one porcelain command accepts as an --input.
type mediaPolicy struct {
	allowYouTube      bool
	enforceCumulative bool
	// classes limits the accepted content classes; nil accepts every sendable one.
	classes []contentClass
	// accepts names those classes in the rejection ("audio or video"), and
	// hint points at the command that takes the rest.
	accepts string
	hint    string
}

var (
	analyzePolicy    = mediaPolicy{allowYouTube: true, enforceCumulative: true}
	transcribePolicy = mediaPolicy{
		classes: []contentClass{contentAudio, contentVideo},
		accepts: "audio or video",
		hint:    "Use \"gemini-api analyze\" for images, documents, and text",
	}
)

// checkClass rejects a MIME type whose content class the command does not take.
func (p mediaPolicy) checkClass(mimeType, ref string) error {
	if p.classes == nil {
		return nil
	}
	class, _ := contentClassOf(mimeType)
	if slices.Contains(p.classes, class) {
		return nil
	}
	return usageError(fmt.Sprintf("%s: only %s inputs are accepted; got %s", ref, p.accepts, mimeType), p.hint)
}

// mediaSource is the resolved input of analyze/transcribe: a request content
// block plus a human label for progress lines and artifact names.
type mediaSource struct {
	// content is the ready block of a URI-backed source (files/<id>, YouTube).
	content interactions.Content
	// path is set instead for a local file, which block reads on demand.
	path     string
	ref      string
	label    string
	mimeType string
	// inlineBytes is what block found the local file to cost once read.
	inlineBytes int64
}

// block returns the interaction content block. A local file is read and
// base64-encoded here rather than at resolution, so a multi-input transcribe
// holds one payload at a time instead of all of them.
func (m *mediaSource) block(cmd *cobra.Command) (interactions.Content, error) {
	if m.path == "" {
		return m.content, nil
	}
	class, _ := contentClassOf(m.mimeType)
	var data []byte
	if isDryRun(cmd) {
		// No bytes leave the machine under --dry-run; keep the preview
		// readable instead of streaming the payload to stderr.
		info, err := os.Stat(m.path)
		if err != nil {
			return interactions.Content{}, usageError(fmt.Sprintf("%s: cannot read file: %v", m.ref, err))
		}
		data = fmt.Appendf(nil, "<bytes:%d>", info.Size())
	} else {
		raw, err := os.ReadFile(m.path)
		if err != nil {
			return interactions.Content{}, usageError(fmt.Sprintf("%s: cannot read file: %v", m.ref, err))
		}
		// The file may have grown since validation sized it.
		m.inlineBytes = inlineCost(int64(len(raw)), class)
		if m.inlineBytes > maxInlineBytes {
			return interactions.Content{}, usageError(fmt.Sprintf("%s: file grew past the inline limit while the command ran", m.ref))
		}
		data = raw
		if class != contentText {
			data = base64.StdEncoding.AppendEncode(nil, raw)
		}
	}
	if class == contentText {
		return textContentBlock(string(data)), nil
	}
	return mediaContentBlock(m.mimeType, string(data), true), nil
}

// mediaContentBlock builds an interactions Content block for a media input,
// choosing the union member by MIME class. payload is base64 bytes when inline,
// or a URI otherwise. A Files API resource of unknown type travels as a
// document block without a MIME type.
func mediaContentBlock(mimeType, payload string, inline bool) interactions.Content {
	set := func(data, uri **string) {
		if inline {
			*data = stringPtr(payload)
		} else {
			*uri = stringPtr(payload)
		}
	}
	switch class, _ := contentClassOf(mimeType); class {
	case contentImage:
		c := interactions.ImageContent{MimeType: interactions.ImageContentMimeType(mimeType).ToPointer()}
		set(&c.Data, &c.URI)
		return interactions.CreateContentImage(c)
	case contentAudio:
		c := interactions.AudioContent{MimeType: interactions.AudioContentMimeType(mimeType).ToPointer()}
		set(&c.Data, &c.URI)
		return interactions.CreateContentAudio(c)
	case contentVideo:
		c := interactions.VideoContent{MimeType: interactions.VideoContentMimeType(mimeType).ToPointer()}
		set(&c.Data, &c.URI)
		return interactions.CreateContentVideo(c)
	default:
		c := interactions.DocumentContent{}
		if mimeType != "" {
			c.MimeType = interactions.DocumentContentMimeType(mimeType).ToPointer()
		}
		set(&c.Data, &c.URI)
		return interactions.CreateContentDocument(c)
	}
}

var (
	youtubeHost = regexp.MustCompile(`^(www\.|m\.|music\.)?(youtube\.com|youtu\.be)$`)
	// fileIDShape is the Files API id contract: up to 40 lowercase
	// alphanumerics or dashes, not led or ended by a dash. It is a single path
	// segment, so "." and ".." never reach the /files/{file} path.
	fileIDShape = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
)

// isYouTubeURL reports whether the argument is one of the YouTube URL forms
// the Interactions API accepts as a video content URI.
func isYouTubeURL(arg string) bool {
	if !strings.HasPrefix(arg, "http://") && !strings.HasPrefix(arg, "https://") {
		return false
	}
	u, err := url.Parse(arg)
	if err != nil {
		return false
	}
	return youtubeHost.MatchString(strings.ToLower(u.Host))
}

// normalizeFileID accepts "files/<id>" or a bare id and returns both the
// canonical resource name and the bare id. Returns ok=false for anything
// that does not look like a Files API identifier.
func normalizeFileID(arg string) (name, id string, ok bool) {
	arg = strings.TrimSpace(arg)
	id = strings.TrimPrefix(arg, "files/")
	if id == "" || !fileIDShape.MatchString(id) {
		return "", "", false
	}
	return "files/" + id, id, true
}

// inputRef is the stable label used by validation and API errors. Keeping the
// one-based position visible matters when a repeated --input fails.
func inputRef(index int, arg string) string {
	return fmt.Sprintf("--input[%d] %q", index, arg)
}

// mediaInputKind is what an analyze/transcribe --input value refers to.
type mediaInputKind int

const (
	mediaInputLocal mediaInputKind = iota
	mediaInputRemoteFile
	mediaInputYouTube
)

// classifyMediaInput decides what one --input refers to without touching the
// network. An existing path is always local, even when its spelling starts with
// files/; only an absent, explicit files/<id> value is a Files API reference,
// and bare Files API ids are deliberately not guessed.
func classifyMediaInput(arg, ref string, allowYouTube bool) (mediaInputKind, error) {
	if arg == "" {
		return 0, usageError(ref + ": input cannot be empty")
	}
	if isYouTubeURL(arg) {
		if !allowYouTube {
			return 0, usageError(ref+": YouTube URLs are not supported by transcribe",
				"Download the media locally or upload it with \"gemini-api files upload <path>\"")
		}
		return mediaInputYouTube, nil
	}
	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
		return 0, usageError(ref+": only YouTube URLs are supported as remote inputs",
			"Download the file locally, or upload it with \"gemini-api files upload <path>\" and pass files/<id>")
	}
	_, statErr := os.Stat(arg)
	if statErr == nil {
		return mediaInputLocal, nil
	}
	if errors.Is(statErr, fs.ErrPermission) {
		return 0, usageError(fmt.Sprintf("%s: cannot access file: %v", ref, statErr))
	}
	if !strings.HasPrefix(arg, "files/") {
		return 0, usageError(ref + ": file not found; for an uploaded file pass files/<id>")
	}
	if _, _, ok := normalizeFileID(arg); !ok {
		return 0, usageError(ref + ": invalid Files API reference; expected files/<id>")
	}
	return mediaInputRemoteFile, nil
}

// checkLocalFile enforces what an inline input must satisfy — a non-empty
// regular file within the inline cap whose MIME type the API and the command
// accept — and returns its inline cost and MIME type.
func checkLocalFile(path, mimeOverride, ref string, policy mediaPolicy) (int64, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, "", usageError(ref + ": file not found")
		}
		return 0, "", usageError(fmt.Sprintf("%s: cannot access file: %v", ref, err))
	}
	if !info.Mode().IsRegular() {
		return 0, "", usageError(ref + ": not a regular file")
	}
	if info.Size() == 0 {
		return 0, "", usageError(ref + ": file is empty")
	}
	mimeType := detectMIME(path, mimeOverride)
	if mimeType == "" {
		return 0, "", usageError(ref+": cannot determine the MIME type",
			"Pass --mime-type explicitly (for example --mime-type audio/mp3)")
	}
	class, sendable := contentClassOf(mimeType)
	if !sendable {
		return 0, "", usageError(fmt.Sprintf("%s: MIME type %s is not accepted by the Interactions API", ref, mimeType),
			"Supported inputs: images, audio, video, PDF, CSV, and text files; convert the file, or pass --mime-type if the detection is wrong")
	}
	if err := policy.checkClass(mimeType, ref); err != nil {
		return 0, "", err
	}
	cost := inlineCost(info.Size(), class)
	if cost > maxInlineBytes && class == contentText {
		// Text travels inline only; an uploaded copy could not be referenced.
		return 0, "", usageError(fmt.Sprintf("%s: text file is %d bytes; this CLI sends text inline, up to %d MB per request", ref, info.Size(), maxInlineBytes>>20))
	}
	if cost > maxInlineBytes {
		return 0, "", usageError(
			fmt.Sprintf("%s: file is %d bytes (%d as base64); this CLI keeps inline requests under %d MB", ref, info.Size(), cost, maxInlineRequestBytes>>20),
			fmt.Sprintf("Run \"gemini-api files upload %s\" and pass the returned files/<id> instead", path))
	}
	return cost, mimeType, nil
}

// probeLocalFile confirms the file is readable. A file bound for a text block
// is read in full, since only its content tells text from mislabelled binary.
func probeLocalFile(path, mimeType, ref string) error {
	if class, _ := contentClassOf(mimeType); class == contentText {
		data, err := os.ReadFile(path)
		if err != nil {
			return usageError(fmt.Sprintf("%s: cannot read file: %v", ref, err))
		}
		if !utf8.Valid(data) {
			return usageError(fmt.Sprintf("%s: detected as %s but the content is not UTF-8 text", ref, mimeType),
				"Pass --mime-type with the file's real media type")
		}
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return usageError(fmt.Sprintf("%s: cannot read file: %v", ref, err))
	}
	return f.Close()
}

// prevalidateMediaInputs validates every local input before any response-
// dependent Files API lookup can run. This preserves all-or-nothing probing:
// a bad later path cannot occur after an earlier remote request was previewed
// or sent. Probing each file also catches permissions and mislabelled text.
func prevalidateMediaInputs(inputs []string, mimeOverride string, policy mediaPolicy) error {
	var inlineBytes int64
	for i, raw := range inputs {
		arg := strings.TrimSpace(raw)
		ref := inputRef(i+1, arg)
		kind, err := classifyMediaInput(arg, ref, policy.allowYouTube)
		if err != nil {
			return err
		}
		if kind != mediaInputLocal {
			continue
		}
		cost, mimeType, err := checkLocalFile(arg, mimeOverride, ref, policy)
		if err != nil {
			return err
		}
		if err := probeLocalFile(arg, mimeType, ref); err != nil {
			return err
		}
		inlineBytes += cost
		if policy.enforceCumulative && inlineBytes > maxInlineBytes {
			return usageError(
				fmt.Sprintf("%s: cumulative inline inputs (base64 included) exceed this CLI's %d MB request budget", ref, maxInlineRequestBytes>>20),
				fmt.Sprintf("Run \"gemini-api files upload %s\" and pass the returned files/<id> instead", arg))
		}
	}
	return nil
}

// resolveMediaSources is the shared front half of analyze/transcribe: it
// validates every --input, builds the client, and resolves each input in order.
func resolveMediaSources(cmd *cobra.Command, inputs []string, policy mediaPolicy) (*sdk.GeminiAPI, []*mediaSource, error) {
	mimeOverride, _ := flagutil.GetStringFlag(cmd, "mime-type")
	if len(inputs) > 1 && strings.TrimSpace(mimeOverride) != "" {
		return nil, nil, usageError("--mime-type can only be used with exactly one --input")
	}
	if err := prevalidateMediaInputs(inputs, mimeOverride, policy); err != nil {
		return nil, nil, err
	}
	s, err := client.NewClient(cmd)
	if err != nil {
		return nil, nil, err
	}
	sources := make([]*mediaSource, 0, len(inputs))
	for i, input := range inputs {
		src, err := resolveMediaSource(cmd, s, input, mimeOverride, i+1, policy)
		if err != nil {
			return nil, nil, err
		}
		sources = append(sources, src)
	}
	return s, sources, nil
}

// resolveMediaSource turns one analyze/transcribe --input into a request
// part. Existing regular local files win, explicit files/<id> references are
// resolved through files.get, and analyze may additionally allow YouTube
// URLs.
func resolveMediaSource(cmd *cobra.Command, s *sdk.GeminiAPI, arg, mimeOverride string, index int, policy mediaPolicy) (*mediaSource, error) {
	arg = strings.TrimSpace(arg)
	ref := inputRef(index, arg)
	kind, err := classifyMediaInput(arg, ref, policy.allowYouTube)
	if err != nil {
		return nil, err
	}
	switch kind {
	case mediaInputYouTube:
		return &mediaSource{
			content: interactions.CreateContentVideo(interactions.VideoContent{URI: stringPtr(arg)}),
			label:   arg,
		}, nil
	case mediaInputRemoteFile:
		name, id, _ := normalizeFileID(arg)
		return resolveRemoteFile(cmd, s, name, id, mimeOverride, ref, policy)
	}
	_, mimeType, err := checkLocalFile(arg, mimeOverride, ref, policy)
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(arg)
	return &mediaSource{path: arg, ref: ref, label: abs, mimeType: mimeType}, nil
}

// resolveRemoteFile looks a Files API resource up so the request part carries
// its URI and MIME type. Under --dry-run the lookup is previewed and a
// placeholder URI is used so the main request can still be previewed.
func resolveRemoteFile(cmd *cobra.Command, s *sdk.GeminiAPI, name, id, mimeOverride, ref string, policy mediaPolicy) (*mediaSource, error) {
	fileURI := "https://generativelanguage.googleapis.com/v1beta/" + name
	mimeType := strings.ToLower(strings.TrimSpace(mimeOverride))
	if err := checkRemoteMIME(mimeType, ref, policy); err != nil {
		return nil, err // a bad override fails before the lookup is sent
	}
	// The lookup honors --header and is previewed like every other call; its
	// synthetic dry-run response carries no metadata, so the placeholder URI
	// stands in.
	opts, err := callOpts(cmd)
	if err != nil {
		return nil, err
	}
	res, err := s.Files.FilesGet(cmd.Context(), operations.FilesGetRequest{File: id}, opts...)
	if err != nil {
		progress(cmd, "%s: request failed", ref)
		return nil, output.Error(cmd, err)
	}
	if isDryRun(cmd) {
		progress(cmd, "[DRY-RUN] %s: the file's URI and MIME type come from the files.get response; the previewed block is a placeholder.", ref)
	} else {
		if res.File == nil {
			return nil, runtimeError(fmt.Sprintf("%s: the Files API returned no metadata", ref))
		}
		if res.File.URI != nil && *res.File.URI != "" {
			fileURI = *res.File.URI
		}
		if mimeType == "" && res.File.MimeType != nil {
			mimeType = canonicalRemoteMIME(*res.File.MimeType)
		}
		state := genai.State("")
		if res.File.State != nil {
			state = *res.File.State
		}
		switch state {
		case genai.StateActive:
		case genai.StateProcessing:
			return nil, runtimeError(fmt.Sprintf("%s: %s is still processing", ref, name),
				fmt.Sprintf("Wait for it to become ACTIVE: gemini-api files get %s", name))
		case genai.StateFailed:
			return nil, runtimeError(fmt.Sprintf("%s: %s failed processing on the server", ref, name))
		default:
			if state == "" {
				state = "unset"
			}
			return nil, runtimeError(fmt.Sprintf("%s: %s is not ACTIVE (state: %s)", ref, name, state),
				fmt.Sprintf("Check it: gemini-api files get %s", name))
		}
		if mimeType == "" && policy.classes != nil {
			return nil, runtimeError(fmt.Sprintf("%s: the Files API reports no MIME type for %s, so it cannot be confirmed as %s", ref, name, policy.accepts),
				"Pass --mime-type explicitly (for example --mime-type audio/mp3)")
		}
	}
	if err := checkRemoteMIME(mimeType, ref, policy); err != nil {
		return nil, err
	}
	return &mediaSource{content: mediaContentBlock(mimeType, fileURI, false), label: name, mimeType: mimeType}, nil
}

// checkRemoteMIME rejects a known MIME type no URI-backed block accepts, or the
// command does not take. A text block cannot reference a URI, so only the typed
// media and document blocks can carry a Files API resource; an unknown ("")
// type is left to the caller.
func checkRemoteMIME(mimeType, ref string, policy mediaPolicy) error {
	if mimeType == "" {
		return nil
	}
	if class, sendable := contentClassOf(mimeType); !sendable || class == contentText {
		return usageError(fmt.Sprintf("%s: MIME type %s cannot be referenced by URI in the Interactions API", ref, mimeType),
			"Supported uploaded inputs: images, audio, video, PDF, and CSV; pass text files as local paths, or --mime-type if the stored type is wrong")
	}
	return policy.checkClass(mimeType, ref)
}

// remoteMIMEAliases maps the canonical spellings the Files API may report to
// the ones the interactions mime_type enums publish.
var remoteMIMEAliases = map[string]string{"video/quicktime": "video/mov"}

// canonicalRemoteMIME lower-cases a Files API MIME type, drops its parameters,
// and folds known aliases onto the interactions spelling.
func canonicalRemoteMIME(mimeType string) string {
	mimeType, _, _ = strings.Cut(strings.ToLower(mimeType), ";")
	mimeType = strings.TrimSpace(mimeType)
	if alias, ok := remoteMIMEAliases[mimeType]; ok {
		return alias
	}
	return mimeType
}
