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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google-gemini/gemini-api-cli/internal/client"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/interactive"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/genai"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/operations"
	"github.com/spf13/cobra"
)

// uploadChunkSize is the resumable-upload chunk size (8 MiB), matching the
// official SDK. Only the final chunk carries the "finalize" command.
const uploadChunkSize = 8 << 20

// newFilesUploadCmd builds the "files upload" command. Upload uses the
// resumable /upload/<version>/files protocol (X-Goog-Upload-* headers), which
// is absent from the OpenAPI document, so it is hand-written over the CLI's
// shared runtime transport rather than a generated operation.
func newFilesUploadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upload <path>",
		Short: "Upload a local file to the Files API (48h TTL)",
		Long:  "Upload a local file and return its reusable files/<id> name. Use that name with\n\"gemini-api analyze\", \"transcribe\", or advanced interaction requests. Uploaded\nfiles expire after 48 hours; the service accepts up to 2 GB per file (50 MB for\nPDFs) and 20 GB per project.\n\nBy default, stdout prints only the files/<id> name.\n\nArguments:\n  <path>  Local file to upload",
		Example: "  gemini-api files upload lecture.mp4                     # → files/abc123\n" +
			"  gemini-api files upload photo.png --display-name \"Cover\"\n" +
			"  gemini-api files upload clip.wav --wait                 # wait until ready",
		Args: cobra.MaximumNArgs(1),
		RunE: runFilesUpload,
	}
	helpMeta(cmd, "MIME type detected from the extension",
		"https://ai.google.dev/gemini-api/docs/files",
		"metadata-only registration via gemini-api files register")
	cmd.Flags().String("display-name", "", "Human-readable display name for the file")
	cmd.Flags().String("mime-type", "", "Override the detected MIME type")
	cmd.Flags().Bool("wait", false, "Wait until the uploaded file is ready for use")
	cmd.Flags().Duration("wait-timeout", 5*time.Minute, "Maximum time to wait when --wait is set")
	cmd.Flags().Lookup("wait-timeout").DefValue = "5m"
	declareInteractive(cmd, interactive.CommandSpec{Args: []interactive.ArgSpec{{
		Name: "path", Summary: "Local file to upload", Required: true,
	}}})
	return cmd
}

func runFilesUpload(cmd *cobra.Command, args []string) error {
	if usageRequested(cmd) {
		return emitUsageKDL(cmd, cmd.OutOrStdout())
	}
	if len(args) == 0 {
		return output.UsageHelpError(cmd, errors.New("missing required argument <path> (a local file to upload)"))
	}
	path := args[0]
	source, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return usageError(fmt.Sprintf("file not found: %s", path))
		}
		return usageError(fmt.Sprintf("cannot read file: %s", path))
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return usageError(fmt.Sprintf("cannot inspect file: %s", path))
	}
	if !info.Mode().IsRegular() {
		return usageError(fmt.Sprintf("not a regular file: %s", path))
	}
	if info.Size() == 0 {
		return usageError(fmt.Sprintf("file is empty: %s", path))
	}
	mimeOverride, _ := flagutil.GetStringFlag(cmd, "mime-type")
	mimeType := detectMIME(path, mimeOverride)
	if mimeType == "" {
		return usageError(fmt.Sprintf("cannot determine the MIME type of %s", path),
			"Pass --mime-type explicitly (for example --mime-type video/mp4)")
	}
	displayName, _ := flagutil.GetStringFlag(cmd, "display-name")
	if strings.TrimSpace(displayName) == "" {
		displayName = filepath.Base(path)
	}

	t, err := newRawTransport(cmd)
	if err != nil {
		return err
	}

	// Step 1: start the resumable session.
	meta := map[string]any{"file": map[string]any{"display_name": displayName}}
	metaBytes, _ := json.Marshal(meta)
	startURL := t.apiURL("upload/" + t.apiVersion + "/files")
	startReq, err := t.newRequest(cmd.Context(), http.MethodPost, startURL, bytes.NewReader(metaBytes))
	if err != nil {
		return err
	}
	startReq.Header.Set("Content-Type", "application/json")
	startReq.Header.Set("X-Goog-Upload-Protocol", "resumable")
	startReq.Header.Set("X-Goog-Upload-Command", "start")
	startReq.Header.Set("X-Goog-Upload-Header-Content-Length", strconv.FormatInt(info.Size(), 10))
	startReq.Header.Set("X-Goog-Upload-Header-Content-Type", mimeType)

	if isDryRun(cmd) {
		startRes, err := t.do(startReq)
		if err != nil {
			return err
		}
		startRes.Body.Close()
		// The real session URL is response-dependent. A stable URL derived
		// from the start endpoint lets the preview show every exact chunk.
		placeholderURL := strings.TrimRight(startURL, "/") + "/dry-run-session"
		if err := t.previewUploadChunks(cmd.Context(), placeholderURL, source, path, info.Size()); err != nil {
			return err
		}
		if wait, _ := flagutil.GetBoolFlag(cmd, "wait"); wait {
			progress(cmd, "[DRY-RUN] --wait polling is response-dependent and was not simulated.")
		}
		return nil
	}

	progress(cmd, "Starting upload of %s (%d bytes, %s)...", path, info.Size(), mimeType)
	startRes, err := t.do(startReq)
	if err != nil {
		return output.Error(cmd, err)
	}
	uploadURL := startRes.Header.Get("X-Goog-Upload-Url")
	io.Copy(io.Discard, startRes.Body)
	startRes.Body.Close()
	if uploadURL == "" {
		return runtimeError("the server did not return an upload URL")
	}
	if !t.sameService(uploadURL) {
		// Name the host only: the session URL carries the upload_id capability.
		host := uploadURL
		if parsed, err := url.Parse(uploadURL); err == nil && parsed.Host != "" {
			host = parsed.Scheme + "://" + parsed.Host
		}
		return runtimeError(fmt.Sprintf("refusing to upload to an unexpected host: %s", host))
	}

	// Step 2: stream the bytes in chunks; only the last chunk finalizes.
	file, err := t.uploadBytes(cmd.Context(), uploadURL, source, path, info.Size())
	if err != nil {
		if r, ok := err.(rawErr); ok {
			return output.Error(cmd, r.err)
		}
		return runtimeError(err.Error())
	}
	if file.Name == nil || *file.Name == "" {
		return runtimeError("upload finished but the response had no file name")
	}
	name := *file.Name
	progress(cmd, "Uploaded %s as %s.", path, name)

	if wait, _ := flagutil.GetBoolFlag(cmd, "wait"); wait {
		waited, err := t.waitActive(cmd, name)
		if err != nil {
			return err
		}
		if waited != nil {
			file = *waited
		}
	}
	return emitResult(cmd, name, fileEnvelope(&file))
}

// previewUploadChunks reuses the source handle opened before the start request
// but does not read it. The known file size is enough to emit the same offsets,
// lengths, commands, and binary body markers that the live loop would send.
func (t *rawTransport) previewUploadChunks(ctx context.Context, uploadURL string, source *os.File, path string, size int64) error {
	if _, err := source.Stat(); err != nil {
		return fmt.Errorf("cannot inspect %s: %w", path, err)
	}

	for offset := int64(0); offset < size; {
		n := int64(uploadChunkSize)
		if remaining := size - offset; remaining < n {
			n = remaining
		}
		command := "upload"
		if offset+n == size {
			command = "upload, finalize"
		}
		req, err := t.newRequest(ctx, http.MethodPost, uploadURL, client.NewDryRunBody(n))
		if err != nil {
			return err
		}
		req.ContentLength = n
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Length", strconv.FormatInt(n, 10))
		req.Header.Set("X-Goog-Upload-Command", command)
		req.Header.Set("X-Goog-Upload-Offset", strconv.FormatInt(offset, 10))
		res, err := t.do(req)
		if err != nil {
			return err
		}
		res.Body.Close()
		offset += n
	}
	return nil
}

// rawErr marks an error that already carries an SDK-classified API error so
// the caller routes it through output.Error.
type rawErr struct{ err error }

func (r rawErr) Error() string { return r.err.Error() }

// uploadBytes streams the already-open source to the resumable session URL in
// chunks. Exactly the declared size is sent: bytes appended after the start
// request are ignored, and a source that shrank fails before finalizing.
func (t *rawTransport) uploadBytes(ctx context.Context, uploadURL string, source io.Reader, path string, size int64) (genai.File, error) {
	f := io.LimitReader(source, size)
	buf := make([]byte, uploadChunkSize)
	var offset int64
	var lastBody []byte
	for {
		n, readErr := io.ReadFull(f, buf)
		// "finalize" must ride the last chunk. Detect the end by byte count
		// (offset+n == size), not just io.EOF: a file whose size is an exact
		// multiple of the chunk size fills the buffer with readErr == nil on
		// its final chunk, so an EOF-only check would send that chunk as a
		// plain "upload" and never finalize.
		final := false
		switch {
		case readErr == nil:
			final = offset+int64(n) >= size
		case readErr == io.EOF || readErr == io.ErrUnexpectedEOF:
			if offset+int64(n) != size {
				return genai.File{}, fmt.Errorf("%s changed during upload: read %d of %d bytes", path, offset+int64(n), size)
			}
			final = true
		default:
			return genai.File{}, fmt.Errorf("reading %s at offset %d: %w", path, offset, readErr)
		}
		command := "upload"
		if final {
			command = "upload, finalize"
		}
		req, err := t.newRequest(ctx, http.MethodPost, uploadURL, bytes.NewReader(buf[:n]))
		if err != nil {
			return genai.File{}, err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Length", strconv.Itoa(n))
		req.Header.Set("X-Goog-Upload-Command", command)
		req.Header.Set("X-Goog-Upload-Offset", strconv.FormatInt(offset, 10))
		res, err := t.do(req)
		if err != nil {
			return genai.File{}, rawErr{err}
		}
		body, readBodyErr := io.ReadAll(res.Body)
		status := res.Header.Get("X-Goog-Upload-Status")
		res.Body.Close()
		if readBodyErr != nil {
			return genai.File{}, fmt.Errorf("reading the upload response at offset %d: %w", offset, readBodyErr)
		}
		lastBody = body
		offset += int64(n)
		if final {
			if status != "final" {
				return genai.File{}, fmt.Errorf("upload finalized but server status is %q", status)
			}
			break
		}
		if status != "active" {
			return genai.File{}, fmt.Errorf("upload interrupted: server status is %q at offset %d", status, offset)
		}
	}

	var wrapper struct {
		File genai.File `json:"file"`
	}
	if err := json.Unmarshal(lastBody, &wrapper); err != nil {
		return genai.File{}, fmt.Errorf("upload finished but the response was not valid JSON: %w", err)
	}
	return wrapper.File, nil
}

// waitActive polls files.get until the file is ACTIVE, fails, or the
// --wait-timeout deadline passes; the last sleep is cut to that deadline.
func (t *rawTransport) waitActive(cmd *cobra.Command, name string) (*genai.File, error) {
	s, err := client.NewClient(cmd)
	if err != nil {
		return nil, err
	}
	callOpts, err := output.PrepareCallOpts(cmd)
	if err != nil {
		return nil, err
	}
	timeout, _ := cmd.Flags().GetDuration("wait-timeout")
	deadline := time.Now().Add(timeout)
	id := strings.TrimPrefix(name, "files/")
	delay := 2 * time.Second
	for {
		res, err := s.Files.FilesGet(cmd.Context(), operations.FilesGetRequest{File: id}, callOpts...)
		if err != nil {
			return nil, output.Error(cmd, err)
		}
		file := res.File
		if file != nil && file.State != nil {
			switch *file.State {
			case genai.StateActive:
				progress(cmd, "%s is ACTIVE.", name)
				return file, nil
			case genai.StateFailed:
				return nil, runtimeError(fmt.Sprintf("%s failed processing on the server", name))
			case genai.StateProcessing, genai.StateStateUnspecified, "":
			default:
				// A state this CLI does not know will not turn into ACTIVE by waiting.
				return nil, runtimeError(fmt.Sprintf("%s is in unexpected state %s", name, *file.State),
					fmt.Sprintf("Check it: gemini-api files get %s", name))
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, runtimeError(fmt.Sprintf("%s did not become ACTIVE within %s", name, timeout),
				fmt.Sprintf("Check its state later: gemini-api files get %s", name))
		}
		progress(cmd, "Waiting for %s to become ACTIVE...", name)
		select {
		case <-cmd.Context().Done():
			return nil, cmd.Context().Err()
		case <-time.After(min(delay, remaining)):
		}
		if delay < 15*time.Second {
			delay += 2 * time.Second
		}
	}
}

// fileEnvelope flattens a File into the porcelain JSON envelope.
func fileEnvelope(f *genai.File) map[string]any {
	env := map[string]any{}
	if f == nil {
		return env
	}
	if f.Name != nil {
		env["name"] = *f.Name
	}
	if f.URI != nil {
		env["uri"] = *f.URI
	}
	if f.MimeType != nil {
		env["mime_type"] = *f.MimeType
	}
	if f.DisplayName != nil {
		env["display_name"] = *f.DisplayName
	}
	if f.SizeBytes != nil {
		env["size_bytes"] = *f.SizeBytes
	}
	if f.State != nil {
		env["state"] = string(*f.State)
	}
	return env
}
