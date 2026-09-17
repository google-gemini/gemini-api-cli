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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

type apiHTTPError struct {
	StatusCode  int
	Body        string
	RawResponse *http.Response
}

func (e *apiHTTPError) Error() string {
	return fmt.Sprintf("API error: Status %d\n%s", e.StatusCode, e.Body)
}

var apiVersionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type requestContractHook struct{}

func (h *requestContractHook) BeforeRequest(hookCtx BeforeRequestContext, req *http.Request) (*http.Request, error) {
	apiVersion := hookCtx.SDKConfiguration.Globals.APIVersion
	if apiVersion == nil || !apiVersionPattern.MatchString(*apiVersion) {
		return nil, fmt.Errorf("API version is required and must contain only letters, numbers, '.', '_', or '-'; set --api-version, GEMINI_API_VERSION, or globals.api_version in the config file")
	}
	if userProject := hookCtx.SDKConfiguration.Globals.UserProject; userProject != nil && *userProject != "" {
		req.Header.Set("x-goog-user-project", *userProject)
	}
	if req.Header.Get("Authorization") != "" {
		req.Header.Del("x-goog-api-key")
	}
	if hookCtx.OperationID == "ModelsStreamGenerateContent" {
		query := req.URL.Query()
		query.Set("alt", "sse")
		req.URL.RawQuery = query.Encode()
		req.Header.Set("Accept", "text/event-stream")
	}
	if isStreamingInteraction(hookCtx.OperationID, req) {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req, nil
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
