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

	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/sdkerrors"
	"github.com/google-gemini/gemini-api-cli/internal/usage"
	"github.com/spf13/cobra"
)

// environmentArchiveMIME supplements the shared MIME tables in media.go with
// archive and config formats commonly uploaded into sandbox environments.
var environmentArchiveMIME = map[string]string{
	".tar":   "application/x-tar",
	".gz":    "application/gzip",
	".tgz":   "application/gzip",
	".zip":   "application/zip",
	".jsonl": "application/jsonl",
	".toml":  "application/toml",
	".sh":    "application/x-sh",
}

func newEnvironmentFilesUploadCmd() *cobra.Command {
	var (
		envFlag       string
		fileFlag      string
		pathFlag      string
		mimeFlag      string
		overwriteFlag bool
		extractFlag   bool
	)

	cmd := &cobra.Command{
		Use:   "upload <environment> <local-file>",
		Short: "Upload a local file or archive into an environment",
		Long: "Upload a local file (or a .tar/.tar.gz archive with --extract) into an environment using the resumable upload protocol.\n\n" +
			"The destination path inside the environment defaults to the basename of <local-file> unless overridden with --path. " +
			"The MIME type is inferred from the file extension unless overridden with --mime-type.\n\n" +
			"Prints the destination path on stdout in default mode, or a structured envelope with -o json.",
		Example: "  gemini-api environments files upload env_abc123 ./hello.txt\n" +
			"  gemini-api environments files upload environments/env_abc123 ./main.py --path src/main.py --overwrite\n" +
			"  gemini-api environments files upload env_abc123 ./bundle.tar.gz --path app --extract --overwrite",
		Annotations: map[string]string{
			"speakeasy_args_help": "<environment> <local-file>",
		},
		Args: func(cmd *cobra.Command, args []string) error {
			if usage.UsageRequested(cmd) {
				return nil
			}
			env, localFile, extra := resolveEnvUploadArgs(args, envFlag, fileFlag)
			if extra {
				return usageError(fmt.Sprintf("accepts 2 arguments (<environment> <local-file>), received %d", len(args)))
			}
			if env == "" {
				return usageError("missing environment; usage: gemini-api environments files upload <environment> <local-file>")
			}
			if localFile == "" {
				return usageError("missing local file path; usage: gemini-api environments files upload <environment> <local-file>")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			envRaw, localFile, _ := resolveEnvUploadArgs(args, envFlag, fileFlag)
			return runEnvironmentFilesUpload(cmd, envRaw, localFile, pathFlag, mimeFlag, overwriteFlag, extractFlag)
		},
	}

	cmd.Flags().StringVarP(&pathFlag, "path", "p", "", "Destination path inside the environment (default: basename of <local-file>)")
	cmd.Flags().StringVar(&mimeFlag, "mime-type", "", "MIME type override (inferred from file extension when omitted)")
	cmd.Flags().BoolVar(&overwriteFlag, "overwrite", false, "Overwrite existing file(s) at the destination path")
	cmd.Flags().BoolVar(&extractFlag, "extract", false, "Extract a caller-provided .tar or .tar.gz archive into the destination directory")

	// Hidden flags allow flag-oriented callers to pass --environment / --file.
	cmd.Flags().StringVarP(&envFlag, "environment", "e", "", "Environment ID or environments/<id>")
	_ = cmd.Flags().MarkHidden("environment")
	cmd.Flags().StringVarP(&fileFlag, "file", "f", "", "Local file path to upload")
	_ = cmd.Flags().MarkHidden("file")

	return cmd
}

func newEnvironmentFilesDownloadCmd() *cobra.Command {
	var (
		envFlag  string
		pathFlag string
		outFlag  string
	)

	cmd := &cobra.Command{
		Use:   "download <environment> <path>",
		Short: "Download a single file's raw content from an environment",
		Long: "Download a single file's raw content from an environment with alt=media and write the bytes to disk.\n\n" +
			"The output path defaults to ./<basename>; when --out names an existing directory (or ends with a path separator), " +
			"the file is written to <dir>/<basename>. Prints the written local path on stdout without emitting binary content to the terminal.\n\n" +
			"To inspect directory contents, use \"gemini-api environments files list --recursive\".",
		Example: "  gemini-api environments files download env_abc123 src/main.py\n" +
			"  gemini-api environments files download environments/env_abc123 src/main.py --out ./downloads/\n" +
			"  gemini-api environments files download env_abc123 config.json --out ./local-config.json",
		Annotations: map[string]string{
			"speakeasy_args_help": "<environment> <path>",
		},
		Args: func(cmd *cobra.Command, args []string) error {
			if usage.UsageRequested(cmd) {
				return nil
			}
			env, remotePath, extra := resolveEnvDownloadArgs(args, envFlag, pathFlag)
			if extra {
				return usageError(fmt.Sprintf("accepts 2 arguments (<environment> <path>), received %d", len(args)))
			}
			if env == "" {
				return usageError("missing environment; usage: gemini-api environments files download <environment> <path>")
			}
			if remotePath == "" {
				return usageError("missing file path; usage: gemini-api environments files download <environment> <path>")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			envRaw, remotePath, _ := resolveEnvDownloadArgs(args, envFlag, pathFlag)
			return runEnvironmentFilesDownload(cmd, envRaw, remotePath, outFlag)
		},
	}

	cmd.Flags().StringVar(&outFlag, "out", "", "Write the downloaded file to this path or into this directory (default: ./<basename>)")

	// Hidden flags allow flag-oriented callers to pass --environment / --path.
	cmd.Flags().StringVarP(&envFlag, "environment", "e", "", "Environment ID or environments/<id>")
	_ = cmd.Flags().MarkHidden("environment")
	cmd.Flags().StringVarP(&pathFlag, "path", "p", "", "File path inside the environment")
	_ = cmd.Flags().MarkHidden("path")

	return cmd
}

func resolveEnvUploadArgs(args []string, envFlag, fileFlag string) (env, localFile string, extra bool) {
	env = strings.TrimSpace(envFlag)
	localFile = strings.TrimSpace(fileFlag)
	switch len(args) {
	case 0:
		return env, localFile, false
	case 1:
		if env == "" {
			env = strings.TrimSpace(args[0])
		} else if localFile == "" {
			localFile = strings.TrimSpace(args[0])
		} else {
			return env, localFile, true
		}
		return env, localFile, false
	case 2:
		if env != "" || localFile != "" {
			return env, localFile, true
		}
		return strings.TrimSpace(args[0]), strings.TrimSpace(args[1]), false
	default:
		return env, localFile, true
	}
}

func resolveEnvDownloadArgs(args []string, envFlag, pathFlag string) (env, remotePath string, extra bool) {
	env = strings.TrimSpace(envFlag)
	remotePath = strings.TrimSpace(pathFlag)
	switch len(args) {
	case 0:
		return env, remotePath, false
	case 1:
		if env == "" {
			env = strings.TrimSpace(args[0])
		} else if remotePath == "" {
			remotePath = strings.TrimSpace(args[0])
		} else {
			return env, remotePath, true
		}
		return env, remotePath, false
	case 2:
		if env != "" || remotePath != "" {
			return env, remotePath, true
		}
		return strings.TrimSpace(args[0]), strings.TrimSpace(args[1]), false
	default:
		return env, remotePath, true
	}
}

func runEnvironmentFilesUpload(cmd *cobra.Command, envRaw, localFile, pathFlag, mimeFlag string, overwriteFlag, extractFlag bool) error {
	envID, err := normalizeEnvironmentID(envRaw)
	if err != nil {
		return usageError(err.Error())
	}

	if localFile == "" {
		return usageError("missing local file path; usage: gemini-api environments files upload <environment> <local-file>")
	}
	info, err := os.Stat(localFile)
	if err != nil {
		return usageError(fmt.Sprintf("cannot stat %q: %v", localFile, err))
	}
	if info.IsDir() {
		return usageError(fmt.Sprintf("%q is a directory; upload expects a regular file (or a .tar/.tar.gz archive with --extract)", localFile))
	}
	if !info.Mode().IsRegular() {
		return usageError(fmt.Sprintf("%q is not a regular file", localFile))
	}
	size := info.Size()

	rawDest := strings.TrimSpace(pathFlag)
	if rawDest == "" {
		rawDest = filepath.Base(localFile)
	}
	destPath, err := normalizeEnvironmentCleanPath(rawDest)
	if err != nil {
		return usageError(err.Error())
	}

	mimeType := detectEnvironmentFileMIME(localFile, destPath, mimeFlag)

	t, err := newRawTransport(cmd)
	if err != nil {
		return output.Error(cmd, err)
	}

	startURL := t.apiURL(fmt.Sprintf("upload/%s/environments/%s/files/%s",
		url.PathEscape(t.apiVersion),
		url.PathEscape(envID),
		encodeEnvironmentPath(destPath),
	))
	q := url.Values{}
	if overwriteFlag {
		q.Set("overwrite", "true")
	} else if f := cmd.Flags().Lookup("overwrite"); f != nil && f.Changed {
		q.Set("overwrite", "false")
	}
	if extractFlag {
		q.Set("extract", "true")
	} else if f := cmd.Flags().Lookup("extract"); f != nil && f.Changed {
		q.Set("extract", "false")
	}
	if encodedQuery := q.Encode(); encodedQuery != "" {
		startURL += "?" + encodedQuery
	}

	startReq, err := t.newRequest(cmd.Context(), http.MethodPut, startURL, nil)
	if err != nil {
		return output.Error(cmd, err)
	}
	startReq.Header.Set("X-Goog-Upload-Protocol", "resumable")
	startReq.Header.Set("X-Goog-Upload-Command", "start")
	startReq.Header.Set("X-Goog-Upload-Header-Content-Length", strconv.FormatInt(size, 10))
	startReq.Header.Set("X-Goog-Upload-Header-Content-Type", mimeType)

	// --dry-run previews the start request without uploading bytes.
	if isDryRun(cmd) {
		startRes, err := t.do(startReq)
		if startRes != nil && startRes.Body != nil {
			_ = startRes.Body.Close()
		}
		if err != nil {
			return output.Error(cmd, err)
		}
		return nil
	}

	startRes, err := t.do(startReq)
	if err != nil {
		return output.Error(cmd, err)
	}
	_, _ = io.Copy(io.Discard, startRes.Body)
	_ = startRes.Body.Close()

	uploadURL := strings.TrimSpace(startRes.Header.Get("X-Goog-Upload-Url"))
	if uploadURL == "" {
		return output.Error(cmd, fmt.Errorf("resumable upload start succeeded (HTTP %d) but response omitted X-Goog-Upload-Url header", startRes.StatusCode))
	}
	if !t.sameService(uploadURL) {
		return output.Error(cmd, fmt.Errorf("refusing to send upload bytes to untrusted URL %q", uploadURL))
	}

	f, err := os.Open(localFile)
	if err != nil {
		return output.Error(cmd, fmt.Errorf("open %q: %w", localFile, err))
	}
	defer f.Close()

	finalBody, err := uploadEnvironmentChunks(cmd, t, uploadURL, f, size, mimeType)
	if err != nil {
		return output.Error(cmd, err)
	}

	printedPath, envelope := buildEnvironmentUploadResult(envID, destPath, extractFlag, finalBody)
	return emitResult(cmd, printedPath, envelope)
}

func uploadEnvironmentChunks(cmd *cobra.Command, t *rawTransport, uploadURL string, r io.Reader, totalSize int64, mimeType string) ([]byte, error) {
	if totalSize == 0 {
		return sendEnvironmentChunk(cmd, t, uploadURL, nil, 0, mimeType, true)
	}
	buf := make([]byte, uploadChunkSize)
	var offset int64
	for offset < totalSize {
		remaining := totalSize - offset
		toRead := int64(len(buf))
		if remaining < toRead {
			toRead = remaining
		}
		n, err := io.ReadFull(r, buf[:toRead])
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("read upload chunk at offset %d: %w", offset, err)
		}
		if n == 0 && offset < totalSize {
			return nil, fmt.Errorf("unexpected EOF reading chunk at offset %d of %d", offset, totalSize)
		}
		chunk := buf[:n]
		final := offset+int64(n) >= totalSize
		body, err := sendEnvironmentChunk(cmd, t, uploadURL, chunk, offset, mimeType, final)
		if err != nil {
			return nil, err
		}
		offset += int64(n)
		if final {
			return body, nil
		}
	}
	return nil, nil
}

func sendEnvironmentChunk(cmd *cobra.Command, t *rawTransport, uploadURL string, chunk []byte, offset int64, mimeType string, final bool) ([]byte, error) {
	command := "upload"
	if final {
		command = "upload, finalize"
	}
	req, err := t.newRequest(cmd.Context(), http.MethodPut, uploadURL, bytes.NewReader(chunk))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(chunk))
	req.Header.Set("Content-Length", strconv.Itoa(len(chunk)))
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("X-Goog-Upload-Offset", strconv.FormatInt(offset, 10))
	req.Header.Set("X-Goog-Upload-Command", command)

	res, err := t.do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("read upload chunk response: %w", err)
	}

	status := strings.TrimSpace(res.Header.Get("X-Goog-Upload-Status"))
	if !final && status != "active" {
		return nil, fmt.Errorf("unexpected X-Goog-Upload-Status %q after intermediate chunk at offset %d (expected \"active\")", status, offset)
	}
	if final && status != "final" {
		return nil, fmt.Errorf("unexpected X-Goog-Upload-Status %q after final chunk (expected \"final\")", status)
	}
	return body, nil
}

func buildEnvironmentUploadResult(envID, destPath string, extractFlag bool, finalBody []byte) (string, map[string]any) {
	printedPath := destPath
	envelope := map[string]any{
		"environment": "environments/" + envID,
		"path":        destPath,
	}
	trimmed := bytes.TrimSpace(finalBody)
	if len(trimmed) == 0 {
		return printedPath, envelope
	}
	var parsed map[string]any
	if err := json.Unmarshal(trimmed, &parsed); err != nil || parsed == nil {
		return printedPath, envelope
	}
	for k, v := range parsed {
		envelope[k] = v
	}
	if !extractFlag {
		if p, ok := parsed["path"].(string); ok && strings.TrimSpace(p) != "" {
			printedPath = strings.TrimSpace(p)
		} else if fileObj, ok := parsed["file"].(map[string]any); ok {
			if p, ok := fileObj["path"].(string); ok && strings.TrimSpace(p) != "" {
				printedPath = strings.TrimSpace(p)
			}
		} else if filesArr, ok := parsed["files"].([]any); ok && len(filesArr) == 1 {
			if first, ok := filesArr[0].(map[string]any); ok {
				if p, ok := first["path"].(string); ok && strings.TrimSpace(p) != "" {
					printedPath = strings.TrimSpace(p)
				}
			}
		}
	}
	envelope["path"] = printedPath
	if _, ok := envelope["environment"]; !ok {
		envelope["environment"] = "environments/" + envID
	}
	return printedPath, envelope
}

func runEnvironmentFilesDownload(cmd *cobra.Command, envRaw, rawPath, outFlag string) error {
	envID, err := normalizeEnvironmentID(envRaw)
	if err != nil {
		return usageError(err.Error())
	}

	trimmedPath := strings.TrimSpace(rawPath)
	if trimmedPath == "" {
		return usageError("missing file path; usage: gemini-api environments files download <environment> <path>")
	}
	// Reject obvious directory paths before sending any request.
	if trimmedPath == "/" || trimmedPath == "." || strings.HasSuffix(trimmedPath, "/") {
		return usageError(directoryDownloadErrorMsg(envID, trimmedPath))
	}

	cleanPath, err := normalizeEnvironmentCleanPath(trimmedPath)
	if err != nil {
		return usageError(err.Error())
	}

	t, err := newRawTransport(cmd)
	if err != nil {
		return output.Error(cmd, err)
	}

	downloadURL := t.apiURL(fmt.Sprintf("%s/environments/%s/files/%s?alt=media",
		url.PathEscape(t.apiVersion),
		url.PathEscape(envID),
		encodeEnvironmentPath(cleanPath),
	))
	req, err := t.newRequest(cmd.Context(), http.MethodGet, downloadURL, nil)
	if err != nil {
		return output.Error(cmd, err)
	}
	req.Header.Set("Accept", "application/octet-stream, */*")

	if isDryRun(cmd) {
		res, err := t.do(req)
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}
		if err != nil {
			return output.Error(cmd, err)
		}
		return nil
	}

	res, err := t.do(req)
	if err != nil {
		if isDirectoryDownloadError(err) {
			return usageError(directoryDownloadErrorMsg(envID, cleanPath))
		}
		return output.Error(cmd, err)
	}
	defer res.Body.Close()

	var bodyReader io.Reader = res.Body
	if strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "application/json") {
		sniff, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		if err != nil {
			return output.Error(cmd, fmt.Errorf("read downloaded file %q: %w", cleanPath, err))
		}
		if looksLikeDirectoryListingResponse(res.Header.Get("Content-Type"), sniff, cleanPath) {
			return usageError(directoryDownloadErrorMsg(envID, cleanPath))
		}
		bodyReader = io.MultiReader(bytes.NewReader(sniff), res.Body)
	}

	localPath := resolveEnvironmentDownloadPath(outFlag, filepath.Base(cleanPath))
	written, err := writeArtifactStream(localPath, bodyReader, cleanPath)
	if err != nil {
		return output.Error(cmd, err)
	}

	return emitResult(cmd, localPath, map[string]any{
		"environment": "environments/" + envID,
		"path":        cleanPath,
		"output_path": localPath,
		"size_bytes":  int(written),
	})
}

func writeArtifactStream(path string, r io.Reader, cleanPath string) (int64, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	written, err := io.Copy(tmp, r)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return 0, fmt.Errorf("read downloaded file %q: %w", cleanPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return 0, err
	}
	return written, nil
}

func resolveEnvironmentDownloadPath(outFlag, basename string) string {
	out := strings.TrimSpace(outFlag)
	if out == "" {
		return "./" + basename
	}
	if namesDirectory(out) {
		return filepath.Join(out, basename)
	}
	return out
}

func directoryDownloadErrorMsg(envID, rawPath string) string {
	dirHint := strings.Trim(strings.TrimSpace(rawPath), "/")
	if dirHint == "" {
		dirHint = "."
	}
	return fmt.Sprintf("path %q refers to a directory; use \"gemini-api environments files list --environment %s --path %s --recursive\" to list directory contents", rawPath, envID, dirHint)
}

func isDirectoryDownloadError(err error) bool {
	var sdkErr *sdkerrors.SDKDefaultError
	if !errors.As(err, &sdkErr) || sdkErr == nil {
		return false
	}
	bodyLower := strings.ToLower(sdkErr.Body)
	if strings.Contains(bodyLower, "directory") || strings.Contains(bodyLower, "not a regular file") {
		return true
	}
	if sdkErr.StatusCode == http.StatusBadRequest || sdkErr.StatusCode == http.StatusUnprocessableEntity {
		if strings.Contains(bodyLower, "failed_precondition") || strings.Contains(bodyLower, "invalid_argument") {
			return true
		}
	}
	return false
}

func looksLikeDirectoryListingResponse(contentType string, data []byte, requestedPath string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if !strings.Contains(ct, "application/json") {
		return false
	}
	var listing struct {
		Files []struct {
			Name        string `json:"name"`
			Path        string `json:"path"`
			IsDirectory bool   `json:"is_directory"`
		} `json:"files"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &listing); err != nil || len(listing.Files) == 0 {
		return false
	}
	for _, entry := range listing.Files {
		if !strings.HasPrefix(entry.Name, "environments/") {
			return false
		}
		if entry.Path == requestedPath && entry.IsDirectory {
			return true
		}
		if strings.HasPrefix(entry.Path, requestedPath+"/") {
			return true
		}
	}
	return len(listing.Files) > 1
}

func normalizeEnvironmentCleanPath(p string) (string, error) {
	cleaned, err := normalizeEnvironmentFilePath(p)
	if err != nil {
		return "", err
	}
	cleaned = strings.TrimRight(cleaned, "/")
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "", fmt.Errorf("invalid path %q; expected a relative path inside the environment (e.g. \"src/main.py\")", strings.TrimSpace(p))
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("invalid path %q; path segments must not be empty, \".\", or \"..\"", strings.TrimSpace(p))
		}
	}
	return cleaned, nil
}

func encodeEnvironmentPath(cleanPath string) string {
	parts := strings.Split(cleanPath, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func detectEnvironmentFileMIME(localPath, destPath, override string) string {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		return strings.ToLower(trimmed)
	}
	if m := lookupEnvironmentMIMEByPath(localPath); m != "" {
		return m
	}
	if m := lookupEnvironmentMIMEByPath(destPath); m != "" {
		return m
	}
	return "application/octet-stream"
}

func lookupEnvironmentMIMEByPath(p string) string {
	lower := strings.ToLower(strings.TrimSpace(p))
	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
		return "application/gzip"
	}
	ext := strings.ToLower(filepath.Ext(lower))
	if ext == "" {
		return ""
	}
	if m, ok := mimeByExtension[ext]; ok {
		return m
	}
	if m, ok := uploadOnlyMIME[ext]; ok {
		return m
	}
	if m, ok := environmentArchiveMIME[ext]; ok {
		return m
	}
	return ""
}
