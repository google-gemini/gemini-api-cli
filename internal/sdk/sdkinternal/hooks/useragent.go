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
	"fmt"
	"runtime"
	"strings"

	"github.com/google-gemini/gemini-api-cli/internal/sdk/sdkinternal/config"
)

// UserAgent formats the CLI User-Agent string following the google-genai
// client convention: {product}/{version} gl-{lang}/{runtime},
// e.g. "google-genai-cli/0.5.4 gl-go/go1.24.0".
func UserAgent(version string) string {
	return fmt.Sprintf("google-genai-cli/%s gl-go/%s", strings.TrimSpace(version), runtime.Version())
}

// userAgentHook rewrites the default Speakeasy User-Agent into the
// google-genai client convention.
type userAgentHook struct{}

func (userAgentHook) SDKInit(cfg config.SDKConfiguration) config.SDKConfiguration {
	cfg.UserAgent = UserAgent(cfg.SDKVersion)
	return cfg
}
