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
	"errors"
	"io"
	"mime"
	"net/http"
)

type interactionErrorHook struct{}

func (h *interactionErrorHook) AfterSuccess(hookCtx AfterSuccessContext, res *http.Response) (*http.Response, error) {
	return normalizeSingletonInteractionError(hookCtx.OperationID, res, nil)
}

func (h *interactionErrorHook) AfterError(hookCtx AfterErrorContext, res *http.Response, err error) (*http.Response, error) {
	originalRes := res
	res, normalizedErr := normalizeSingletonInteractionError(hookCtx.OperationID, res, err)
	var apiErr *apiHTTPError
	if originalRes != nil && res == nil && errors.As(normalizedErr, &apiErr) && apiErr.RawResponse == originalRes {
		return nil, &FailEarly{Cause: apiErr}
	}
	return res, normalizedErr
}

func normalizeSingletonInteractionError(operationID string, res *http.Response, err error) (*http.Response, error) {
	if !isInteractionOperation(operationID) || res == nil || res.StatusCode < http.StatusBadRequest || res.Body == nil || !isJSONContentType(res.Header.Get("Content-Type")) {
		return res, err
	}

	body, readErr := io.ReadAll(res.Body)
	closeErr := res.Body.Close()
	res.Body = io.NopCloser(bytes.NewReader(body))
	if readErr != nil {
		if err != nil {
			return res, err
		}
		return res, readErr
	}
	if closeErr != nil {
		if err != nil {
			return res, err
		}
		return res, closeErr
	}

	var envelope []json.RawMessage
	if json.Unmarshal(body, &envelope) != nil || len(envelope) != 1 {
		return res, err
	}

	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(envelope[0], &payload) != nil || len(payload.Error) == 0 || bytes.Equal(payload.Error, []byte("null")) {
		return res, err
	}

	return nil, &apiHTTPError{
		StatusCode:  res.StatusCode,
		Body:        string(body),
		RawResponse: res,
	}
}

func isInteractionOperation(operationID string) bool {
	switch operationID {
	case "CreateInteraction", "getInteractionById", "deleteInteraction", "cancelInteractionById":
		return true
	default:
		return false
	}
}

func isJSONContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}
