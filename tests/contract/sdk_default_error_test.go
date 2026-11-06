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

package contract_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/operations"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/sdkerrors"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/retry"
	"github.com/google-gemini/gemini-api-cli/internal/sdk"
)

func TestSDKDefaultErrorMessageFromBody(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		want        string
	}{
		{
			name:        "interactions error",
			status:      http.StatusBadRequest,
			contentType: "application/json",
			body:        `{"error":{"message":"Model name cannot be empty.","code":"invalid_request"}}`,
			want:        "Model name cannot be empty.",
		},
		{
			name:        "oneplatform error",
			status:      http.StatusBadRequest,
			contentType: "application/json",
			body:        `{"error":{"code":400,"message":"Bad thing","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID"}]}}`,
			want:        "Bad thing",
		},
		{
			name:        "server error",
			status:      http.StatusBadGateway,
			contentType: "application/json",
			body:        `{"error":{"code":502,"message":"Upstream unavailable","status":"UNAVAILABLE"}}`,
			want:        "Upstream unavailable",
		},
		{
			name:        "non-JSON body",
			status:      http.StatusBadRequest,
			contentType: "text/html",
			body:        "<html>Bad Request</html>",
			want:        "API error occurred",
		},
		{
			name:        "malformed JSON body",
			status:      http.StatusBadRequest,
			contentType: "application/json",
			body:        `{"error":`,
			want:        "API error occurred",
		},
		{
			name:        "no error message",
			status:      http.StatusBadRequest,
			contentType: "application/json",
			body:        `{"error":{"code":"invalid_request"}}`,
			want:        "API error occurred",
		},
		{
			name:        "empty body",
			status:      http.StatusBadRequest,
			contentType: "application/json",
			body:        "",
			want:        "API error occurred",
		},
		{
			name:        "specific message kept",
			status:      http.StatusOK,
			contentType: "text/plain",
			body:        `{"error":{"message":"Model name cannot be empty."}}`,
			want:        "unknown content-type received: text/plain",
		},
	}

	endpoints := []struct {
		name string
		call func(ctx context.Context, client *sdk.GeminiAPI) error
	}{
		{
			name: "Models.ModelsList",
			call: func(ctx context.Context, client *sdk.GeminiAPI) error {
				_, err := client.Models.ModelsList(ctx, &operations.ModelsListRequest{})
				return err
			},
		},
		{
			name: "Agent.List",
			call: func(ctx context.Context, client *sdk.GeminiAPI) error {
				_, err := client.Agent.List(ctx, &operations.ListAgentsRequest{})
				return err
			},
		},
	}

	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", tt.contentType)
						w.WriteHeader(tt.status)
						_, _ = w.Write([]byte(tt.body))
					}))
					defer ts.Close()

					client := sdk.New(
						sdk.WithServerURL(ts.URL),
						sdk.WithAPIVersion("v1beta"),
						sdk.WithRetryConfig(retry.Config{Strategy: "none"}),
					)

					err := ep.call(context.Background(), client)

					var sdkErr *sdkerrors.SDKDefaultError
					if !errors.As(err, &sdkErr) {
						t.Fatalf("%s error = %T (%v), want *sdkerrors.SDKDefaultError", ep.name, err, err)
					}
					if sdkErr.StatusCode != tt.status {
						t.Errorf("StatusCode = %d, want %d", sdkErr.StatusCode, tt.status)
					}
					if sdkErr.Message != tt.want {
						t.Errorf("Message = %q, want %q", sdkErr.Message, tt.want)
					}
					if !strings.HasPrefix(sdkErr.Error(), tt.want+": Status ") {
						t.Errorf("Error() = %q, want prefix %q", sdkErr.Error(), tt.want+": Status ")
					}
				})
			}
		})
	}
}
