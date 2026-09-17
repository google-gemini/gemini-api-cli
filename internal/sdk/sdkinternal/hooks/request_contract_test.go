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
	"net/http"
	"testing"

	"github.com/google-gemini/gemini-api-cli/internal/sdk/sdkinternal/config"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/sdkinternal/globals"
)

func TestModelsStreamGenerateContentRequestContract(t *testing.T) {
	apiVersion := "v1beta"
	req, err := http.NewRequest(http.MethodPost, "https://example.test/v1beta/models/model:streamGenerateContent?key=value&alt=json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")

	hook := &requestContractHook{}
	got, err := hook.BeforeRequest(BeforeRequestContext{HookContext: HookContext{
		OperationID: "ModelsStreamGenerateContent",
		SDKConfiguration: config.SDKConfiguration{Globals: globals.Globals{
			APIVersion: &apiVersion,
		}},
	}}, req)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Query().Get("alt") != "sse" {
		t.Errorf("alt = %q, want sse", got.URL.Query().Get("alt"))
	}
	if got.URL.Query().Get("key") != "value" {
		t.Errorf("existing query parameter was not preserved: %q", got.URL.RawQuery)
	}
	if got.Header.Get("Accept") != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", got.Header.Get("Accept"))
	}
}
