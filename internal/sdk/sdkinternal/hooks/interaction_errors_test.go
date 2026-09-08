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
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
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
