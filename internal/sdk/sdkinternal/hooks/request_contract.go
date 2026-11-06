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

package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/google-gemini/gemini-api-cli/internal/sdk/sdkinternal/config"
)

const (
	maxInlineRequestBytes  = 20 << 20
	inlineRequestLimitHint = `Run "gemini-api files upload <path>" and pass the returned files/<id> instead`
)

type dryRunBodyDeltaKey struct{}

// WithDryRunBodyDelta records the wire-byte difference between the real
// payload and a dry-run placeholder so BeforeRequest validates the actual
// serialised body size.
func WithDryRunBodyDelta(ctx context.Context, delta int64) context.Context {
	if p, ok := ctx.Value(dryRunBodyDeltaKey{}).(*int64); ok {
		*p += delta
		return ctx
	}
	v := delta
	return context.WithValue(ctx, dryRunBodyDeltaKey{}, &v)
}

func consumeDryRunBodyDelta(ctx context.Context) int64 {
	if p, ok := ctx.Value(dryRunBodyDeltaKey{}).(*int64); ok {
		delta := *p
		*p = 0
		return delta
	}
	return 0
}

type apiHTTPError struct {
	StatusCode  int
	Body        string
	RawResponse *http.Response
}

func (e *apiHTTPError) Error() string {
	return fmt.Sprintf("API error: Status %d\n%s", e.StatusCode, e.Body)
}

type requestValidationError struct {
	message string
	hints   []string
}

func (e *requestValidationError) Error() string {
	return e.message
}

func (e *requestValidationError) CLIReason() string {
	return "CLI_VALIDATION"
}

func (e *requestValidationError) CLIHints() []string {
	return e.hints
}

var apiVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type requestContractHook struct{}

func (h *requestContractHook) SDKInit(cfg config.SDKConfiguration) config.SDKConfiguration {
	if cfg.Globals.APIVersion != nil && (*cfg.Globals.APIVersion == "." || *cfg.Globals.APIVersion == "..") {
		invalid := ""
		cfg.Globals.APIVersion = &invalid
	}
	return cfg
}

func (h *requestContractHook) BeforeRequest(hookCtx BeforeRequestContext, req *http.Request) (*http.Request, error) {
	apiVersion := hookCtx.SDKConfiguration.Globals.APIVersion
	// The pattern alone would admit the traversal segments "." and "..".
	if apiVersion == nil || !apiVersionPattern.MatchString(*apiVersion) || *apiVersion == "." || *apiVersion == ".." {
		return nil, &requestValidationError{
			message: "API version must be non-empty, contain only letters, numbers, '.', '_', or '-', and cannot be '.' or '..'; set --api-version, GEMINI_API_VERSION, or globals.api_version in the config file",
		}
	}
	if userProject := hookCtx.SDKConfiguration.Globals.UserProject; userProject != nil && *userProject != "" {
		req.Header.Set("x-goog-user-project", *userProject)
	}
	if req.Header.Get("Authorization") != "" {
		req.Header.Del("x-goog-api-key")
	}
	if err := enforceInteractionRequestSize(hookCtx.OperationID, req); err != nil {
		return nil, err
	}
	if isStreamingInteraction(hookCtx.OperationID, req) {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req, nil
}

func enforceInteractionRequestSize(operationID string, req *http.Request) error {
	if operationID != "CreateInteraction" || req == nil {
		return nil
	}
	delta := consumeDryRunBodyDelta(req.Context())
	var size int64
	switch {
	case req.ContentLength > 0:
		size = req.ContentLength
	case req.GetBody != nil:
		bodyCopy, err := req.GetBody()
		if err != nil {
			return err
		}
		defer bodyCopy.Close()
		n, err := io.Copy(io.Discard, io.LimitReader(bodyCopy, maxInlineRequestBytes+1))
		if err != nil {
			return err
		}
		size = n
	}
	totalSize := size + delta
	if totalSize > maxInlineRequestBytes {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return &requestValidationError{
			message: fmt.Sprintf("request body is %d bytes, which exceeds this CLI's %d MB request budget; this CLI keeps inline requests under %d MB", totalSize, maxInlineRequestBytes>>20, maxInlineRequestBytes>>20),
			hints:   []string{inlineRequestLimitHint},
		}
	}
	return nil
}

func isStreamingInteraction(operationID string, req *http.Request) bool {
	switch operationID {
	case "getInteractionById":
		return req.URL.Query().Get("stream") == "true"
	case "CreateInteraction":
		if req.GetBody == nil {
			return false
		}
		body, err := req.GetBody()
		if err != nil {
			return false
		}
		defer body.Close()
		var payload struct {
			Stream bool `json:"stream"`
		}
		return json.NewDecoder(body).Decode(&payload) == nil && payload.Stream
	default:
		return false
	}
}

func (h *requestContractHook) AfterSuccess(_ AfterSuccessContext, res *http.Response) (*http.Response, error) {
	if res == nil {
		return nil, nil
	}

	isSuccess := res.StatusCode >= http.StatusOK && res.StatusCode < http.StatusMultipleChoices
	isGeneratedError := res.StatusCode >= http.StatusBadRequest
	if isSuccess || isGeneratedError {
		return res, nil
	}
	if res.Body == nil {
		return nil, &apiHTTPError{StatusCode: res.StatusCode, RawResponse: res}
	}

	body, readErr := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if readErr != nil {
		return res, readErr
	}
	res.Body = io.NopCloser(bytes.NewReader(body))

	return nil, &apiHTTPError{
		StatusCode:  res.StatusCode,
		Body:        string(body),
		RawResponse: res,
	}
}
