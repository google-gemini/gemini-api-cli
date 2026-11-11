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
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/google-gemini/gemini-api-cli/internal/sdk/sdkinternal/config"
)

type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return nil
}

type failingBody struct {
	closed bool
}

func (b *failingBody) Read([]byte) (int, error) {
	return 0, errors.New("read body")
}

func (b *failingBody) Close() error {
	b.closed = true
	return nil
}

func TestNormalizeSingletonInteractionError(t *testing.T) {
	const arrayBody = `[{"error":{"code":403,"message":"insufficient scope"}}]`
	sentinel := errors.New("original error")

	tests := []struct {
		name        string
		operationID string
		statusCode  int
		contentType string
		body        string
		wantHandled bool
	}{
		{
			name:        "singleton interaction error array",
			operationID: "CreateInteraction",
			statusCode:  http.StatusForbidden,
			contentType: "application/json; charset=utf-8",
			body:        arrayBody,
			wantHandled: true,
		},
		{
			name:        "get interaction operation",
			operationID: "getInteractionById",
			statusCode:  http.StatusInternalServerError,
			contentType: "application/json",
			body:        arrayBody,
			wantHandled: true,
		},
		{
			name:        "delete interaction operation",
			operationID: "deleteInteraction",
			statusCode:  http.StatusForbidden,
			contentType: "application/json",
			body:        arrayBody,
			wantHandled: true,
		},
		{
			name:        "cancel interaction operation",
			operationID: "cancelInteractionById",
			statusCode:  http.StatusForbidden,
			contentType: "application/json",
			body:        arrayBody,
			wantHandled: true,
		},
		{
			name:        "non-interaction operation",
			operationID: "listAgents",
			statusCode:  http.StatusForbidden,
			contentType: "application/json",
			body:        arrayBody,
		},
		{
			name:        "successful response",
			operationID: "CreateInteraction",
			statusCode:  http.StatusOK,
			contentType: "application/json",
			body:        arrayBody,
		},
		{
			name:        "non-JSON response",
			operationID: "CreateInteraction",
			statusCode:  http.StatusForbidden,
			contentType: "text/plain",
			body:        arrayBody,
		},
		{
			name:        "vendor JSON response",
			operationID: "CreateInteraction",
			statusCode:  http.StatusForbidden,
			contentType: "application/problem+json",
			body:        arrayBody,
		},
		{
			name:        "object response",
			operationID: "CreateInteraction",
			statusCode:  http.StatusForbidden,
			contentType: "application/json",
			body:        `{"error":{"message":"normal shape"}}`,
		},
		{
			name:        "empty array",
			operationID: "CreateInteraction",
			statusCode:  http.StatusForbidden,
			contentType: "application/json",
			body:        `[]`,
		},
		{
			name:        "malformed JSON",
			operationID: "CreateInteraction",
			statusCode:  http.StatusForbidden,
			contentType: "application/json",
			body:        `[`,
		},
		{
			name:        "null error",
			operationID: "CreateInteraction",
			statusCode:  http.StatusForbidden,
			contentType: "application/json",
			body:        `[{"error":null}]`,
		},
		{
			name:        "multiple array entries",
			operationID: "CreateInteraction",
			statusCode:  http.StatusForbidden,
			contentType: "application/json",
			body:        `[{"error":{"message":"first"}},{"error":{"message":"second"}}]`,
		},
		{
			name:        "singleton array without error",
			operationID: "CreateInteraction",
			statusCode:  http.StatusForbidden,
			contentType: "application/json",
			body:        `[{"message":"not an error envelope"}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalBody := &closeTrackingBody{Reader: strings.NewReader(tt.body)}
			res := &http.Response{
				StatusCode: tt.statusCode,
				Header:     http.Header{"Content-Type": []string{tt.contentType}},
				Body:       originalBody,
			}

			gotRes, gotErr := normalizeSingletonInteractionError(tt.operationID, res, sentinel)
			if tt.wantHandled {
				if gotRes != nil {
					t.Fatalf("response = %#v, want nil", gotRes)
				}
				var apiErr *apiHTTPError
				if !errors.As(gotErr, &apiErr) {
					t.Fatalf("error = %T %v, want *apiHTTPError", gotErr, gotErr)
				}
				if apiErr.StatusCode != tt.statusCode {
					t.Errorf("status code = %d, want %d", apiErr.StatusCode, tt.statusCode)
				}
				if apiErr.Body != tt.body {
					t.Errorf("error body = %q, want %q", apiErr.Body, tt.body)
				}
				if apiErr.RawResponse != res {
					t.Error("raw response was not preserved")
				}
			} else {
				if gotRes != res {
					t.Error("response identity changed")
				}
				if !errors.Is(gotErr, sentinel) {
					t.Errorf("error = %v, want original error", gotErr)
				}
			}

			wantClosed := isInteractionOperation(tt.operationID) && tt.statusCode >= http.StatusBadRequest && isJSONContentType(tt.contentType)
			if originalBody.closed != wantClosed {
				t.Errorf("original body closed = %t, want %t", originalBody.closed, wantClosed)
			}

			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != tt.body {
				t.Errorf("response body = %q, want %q", body, tt.body)
			}
		})
	}
}

func TestNormalizeSingletonInteractionErrorMissingResponseBody(t *testing.T) {
	sentinel := errors.New("original error")
	for _, res := range []*http.Response{
		nil,
		{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": []string{"application/json"}}},
	} {
		gotRes, err := normalizeSingletonInteractionError("CreateInteraction", res, sentinel)
		if gotRes != res {
			t.Fatalf("response = %#v, want %#v", gotRes, res)
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want original error", err)
		}
	}
}

func TestNormalizeSingletonInteractionErrorPreservesIncomingErrorOnReadFailure(t *testing.T) {
	sentinel := errors.New("original error")
	body := &failingBody{}
	res := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}

	gotRes, err := normalizeSingletonInteractionError("CreateInteraction", res, sentinel)
	if gotRes != res {
		t.Fatalf("response = %#v, want original response", gotRes)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want original error", err)
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func TestInteractionErrorHookLifecycleMethods(t *testing.T) {
	const body = `[{"error":{"message":"insufficient scope"}}]`
	newResponse := func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}
	hook := &interactionErrorHook{}

	t.Run("after success", func(t *testing.T) {
		res, err := hook.AfterSuccess(AfterSuccessContext{HookContext: HookContext{OperationID: "CreateInteraction"}}, newResponse())
		if res != nil {
			t.Error("response is non-nil")
		}
		var apiErr *apiHTTPError
		if !errors.As(err, &apiErr) {
			t.Fatalf("error = %T %v, want *apiHTTPError", err, err)
		}
	})

	t.Run("after error", func(t *testing.T) {
		res, err := hook.AfterError(AfterErrorContext{HookContext: HookContext{OperationID: "CreateInteraction"}}, newResponse(), errors.New("original error"))
		if res != nil {
			t.Error("response is non-nil")
		}
		var failEarly *FailEarly
		if !errors.As(err, &failEarly) {
			t.Fatalf("error = %T %v, want *FailEarly", err, err)
		}
		var apiErr *apiHTTPError
		if !errors.As(failEarly.Cause, &apiErr) {
			t.Fatalf("cause = %T %v, want *apiHTTPError", failEarly.Cause, failEarly.Cause)
		}
	})
}

func TestSanitizeInteractionErrorBody(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "passes Unsupported file URI type message through unchanged",
			in:   `{"error":{"code":400,"message":"Unsupported file URI type: files/abc123. File URI must be a File API (e.g. https://generativelanguage.googleapis.com/files/<id>), Youtube (e.g. https://www.youtube.com/watch?v=<id>), or HTTPS (e.g. http://path/to/file).), or a valid gURI (e.g. gs://bucket/object","status":"INVALID_ARGUMENT"}}`,
			want: `{"error":{"code":400,"message":"Unsupported file URI type: files/abc123. File URI must be a File API (e.g. https://generativelanguage.googleapis.com/files/<id>), Youtube (e.g. https://www.youtube.com/watch?v=<id>), or HTTPS (e.g. http://path/to/file).), or a valid gURI (e.g. gs://bucket/object","status":"INVALID_ARGUMENT"}}`,
		},
		{
			name: "redacts leaked internal blobstore URI",
			in:   `{"error":{"code":400,"message":"Failed to load media from blobstore:///genai-api/blobref/global::abcdef0123456789 for video generation","status":"INVALID_ARGUMENT"}}`,
			want: `{"error":{"code":400,"message":"Failed to load media from [internal-file-uri] for video generation","status":"INVALID_ARGUMENT"}}`,
		},
		{
			name: "keeps the separator after a redacted blobstore URI",
			in:   `{"error":{"code":400,"message":"Failed to read blobstore:///genai-api/blobref/global::abcdef0123456789: Unsupported file","status":"INVALID_ARGUMENT"}}`,
			want: `{"error":{"code":400,"message":"Failed to read [internal-file-uri]: Unsupported file","status":"INVALID_ARGUMENT"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(tt.in)),
			}
			gotRes, gotErr := normalizeSingletonInteractionError("CreateInteraction", res, nil)
			if gotErr != nil {
				t.Fatalf("unexpected error: %v", gotErr)
			}
			body, err := io.ReadAll(gotRes.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != tt.want {
				t.Errorf("sanitized body = %q, want %q", string(body), tt.want)
			}
		})
	}
}

func TestRequestContractHookAPIVersionValidation(t *testing.T) {
	h := New()
	req, err := http.NewRequest(http.MethodGet, "https://example.com/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, invalid := range []*string{
		nil,
		func() *string { s := ""; return &s }(),
		func() *string { s := "."; return &s }(),
		func() *string { s := ".."; return &s }(),
		func() *string { s := "../x"; return &s }(),
		func() *string { s := "v1/x"; return &s }(),
		func() *string { s := "v1 beta"; return &s }(),
	} {
		cfg := config.SDKConfiguration{}
		cfg.Globals.APIVersion = invalid
		cfg = h.SDKInit(cfg)
		_, gotErr := h.BeforeRequest(BeforeRequestContext{
			HookContext: HookContext{
				SDKConfiguration: cfg,
				OperationID:      "ModelsList",
			},
		}, req)
		if gotErr == nil {
			t.Fatalf("expected validation error for APIVersion=%v", invalid)
		}
		wrapped := fmt.Errorf("sdk wrapper: %w", gotErr)
		var typed interface{ CLIReason() string }
		if !errors.As(wrapped, &typed) || typed.CLIReason() != "CLI_VALIDATION" {
			t.Errorf("wrapped error %T (%v) did not unwrap to CLIReason()==CLI_VALIDATION", wrapped, wrapped)
		}
	}

	for _, valid := range []string{"v1", "v1beta", "v1alpha", "v1_2-3.4"} {
		v := valid
		cfg := config.SDKConfiguration{}
		cfg.Globals.APIVersion = &v
		cfg = h.SDKInit(cfg)
		gotReq, gotErr := h.BeforeRequest(BeforeRequestContext{
			HookContext: HookContext{
				SDKConfiguration: cfg,
				OperationID:      "ModelsList",
			},
		}, req.Clone(req.Context()))
		if gotErr != nil || gotReq == nil {
			t.Errorf("unexpected error for valid APIVersion=%q: %v", valid, gotErr)
		}
	}
}

type countingReader struct {
	r    io.Reader
	read int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

func TestRequestContractHookCreateInteractionSizeLimit(t *testing.T) {
	h := New()
	v1 := "v1"
	cfg := config.SDKConfiguration{}
	cfg.Globals.APIVersion = &v1
	hookCtx := BeforeRequestContext{
		HookContext: HookContext{
			SDKConfiguration: cfg,
			OperationID:      "CreateInteraction",
		},
	}

	assertValidationErr := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected validation error, got nil")
		}
		wrapped := fmt.Errorf("sdk wrapper: %w", err)
		var reason interface{ CLIReason() string }
		if !errors.As(wrapped, &reason) || reason.CLIReason() != "CLI_VALIDATION" {
			t.Errorf("wrapped error %T (%v) did not unwrap to CLIReason()==CLI_VALIDATION", wrapped, wrapped)
		}
		var hinted interface{ CLIHints() []string }
		if !errors.As(wrapped, &hinted) || len(hinted.CLIHints()) == 0 || !strings.Contains(hinted.CLIHints()[0], "gemini-api files upload") {
			t.Errorf("wrapped error hints = %v, want files upload hint", hinted)
		}
	}

	t.Run("known ContentLength at exact limit passes and preserves body", func(t *testing.T) {
		prefix := `{"stream":true,"input":"`
		suffix := `"}`
		fillLen := maxInlineRequestBytes - len(prefix) - len(suffix)
		payload := make([]byte, maxInlineRequestBytes)
		copy(payload, prefix)
		bytes.Repeat([]byte{'a'}, 1)
		for i := len(prefix); i < len(prefix)+fillLen; i++ {
			payload[i] = 'a'
		}
		copy(payload[len(prefix)+fillLen:], suffix)

		req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/interactions", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		origBody := &closeTrackingBody{Reader: bytes.NewReader(payload)}
		req.Body = origBody

		gotReq, err := h.BeforeRequest(hookCtx, req)
		if err != nil {
			t.Fatalf("unexpected error at exact limit: %v", err)
		}
		if origBody.closed {
			t.Error("original body was unexpectedly closed")
		}
		if got := gotReq.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		readBack, err := io.ReadAll(gotReq.Body)
		if err != nil || int64(len(readBack)) != maxInlineRequestBytes {
			t.Errorf("read back %d bytes (err=%v), want %d", len(readBack), err, maxInlineRequestBytes)
		}
	})

	t.Run("known ContentLength one byte over limit fails and closes body", func(t *testing.T) {
		payload := make([]byte, maxInlineRequestBytes+1)
		req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/interactions", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		origBody := &closeTrackingBody{Reader: bytes.NewReader(payload)}
		req.Body = origBody

		_, err = h.BeforeRequest(hookCtx, req)
		assertValidationErr(t, err)
		if !origBody.closed {
			t.Error("expected original body to be closed on rejection")
		}
	})

	t.Run("unknown ContentLength uses bounded GetBody copy and preserves original body", func(t *testing.T) {
		payload := bytes.Repeat([]byte{'x'}, maxInlineRequestBytes)
		origCounter := &countingReader{r: bytes.NewReader(payload)}
		origBody := &closeTrackingBody{Reader: origCounter}
		var copyBody *closeTrackingBody

		req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/interactions", origBody)
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1
		req.GetBody = func() (io.ReadCloser, error) {
			c := &closeTrackingBody{Reader: bytes.NewReader(payload)}
			copyBody = c
			return c, nil
		}

		gotReq, err := h.BeforeRequest(hookCtx, req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if origCounter.read != 0 || origBody.closed {
			t.Errorf("original body read=%d closed=%v, want untouched", origCounter.read, origBody.closed)
		}
		if copyBody == nil || !copyBody.closed {
			t.Error("expected GetBody copy to be closed")
		}
		replayed, err := gotReq.GetBody()
		if err != nil {
			t.Fatalf("GetBody replay failed: %v", err)
		}
		_ = replayed.Close()
	})

	t.Run("unknown ContentLength over limit stops reading at limit+1 and closes bodies", func(t *testing.T) {
		huge := bytes.Repeat([]byte{'x'}, maxInlineRequestBytes+1024)
		origBody := &closeTrackingBody{Reader: bytes.NewReader(huge)}
		copyCounter := &countingReader{r: bytes.NewReader(huge)}
		copyBody := &closeTrackingBody{Reader: copyCounter}

		req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/interactions", origBody)
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1
		req.GetBody = func() (io.ReadCloser, error) {
			return copyBody, nil
		}

		_, err = h.BeforeRequest(hookCtx, req)
		assertValidationErr(t, err)
		if copyCounter.read != maxInlineRequestBytes+1 {
			t.Errorf("GetBody copy read %d bytes, want bounded read of %d", copyCounter.read, maxInlineRequestBytes+1)
		}
		if !copyBody.closed {
			t.Error("expected GetBody copy to be closed")
		}
		if !origBody.closed {
			t.Error("expected original body to be closed on rejection")
		}
	})

	t.Run("non-CreateInteraction operation ignores inline limit", func(t *testing.T) {
		payload := make([]byte, maxInlineRequestBytes+100)
		req, err := http.NewRequest(http.MethodPost, "https://example.com/upload/v1/files", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		otherCtx := BeforeRequestContext{
			HookContext: HookContext{
				SDKConfiguration: cfg,
				OperationID:      "FilesUpload",
			},
		}
		if _, err := h.BeforeRequest(otherCtx, req); err != nil {
			t.Fatalf("unexpected error on non-CreateInteraction operation: %v", err)
		}
	})

	t.Run("dry-run delta accumulates and is consumed on CreateInteraction size check", func(t *testing.T) {
		body := []byte(`{"model":"gemini-3.8-flash","input":[{"type":"text","text":"\u003cbytes:4000000\u003e"}]}`)
		ctx := WithDryRunBodyDelta(context.Background(), 100)
		ctx = WithDryRunBodyDelta(ctx, maxInlineRequestBytes-int64(len(body))-100+1)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/v1/interactions", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.BeforeRequest(hookCtx, req)
		assertValidationErr(t, err)

		// Second request on the same context starts with a fresh 0 delta.
		req2, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/v1/interactions", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.BeforeRequest(hookCtx, req2); err != nil {
			t.Fatalf("unexpected error after delta was consumed: %v", err)
		}
	})
}

func TestUserAgentBuilderAndHook(t *testing.T) {
	const ver = "0.4.5"
	want := fmt.Sprintf("google-genai-cli/%s gl-go/%s", ver, runtime.Version())
	if got := UserAgent(ver); got != want {
		t.Errorf("UserAgent(%q) = %q, want %q", ver, got, want)
	}
	cfg := New().SDKInit(config.SDKConfiguration{
		UserAgent:  "speakeasy-sdk/go 0.4.5 internal v1beta",
		SDKVersion: ver,
	})
	if cfg.UserAgent != want {
		t.Errorf("SDKInit UserAgent = %q, want %q", cfg.UserAgent, want)
	}
}
