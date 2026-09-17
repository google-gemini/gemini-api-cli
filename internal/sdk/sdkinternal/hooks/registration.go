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

/*
 * This file is only ever generated once on the first generation and then is free to be modified.
 * Any hooks you wish to add should be registered in the initHooks function. Feel free to define
 * your hooks in this file or in separate files in the hooks package.
 *
 * Hooks are registered per SDK instance, and are valid for the lifetime of the SDK instance.
 */

func initHooks(h *Hooks) {
	// 1. Injects google-genai-cli/<version> User-Agent header
	h.registerSDKInitHook(userAgentHook{})
	// 2. Enforces streaming request contract (e.g. Accept: text/event-stream)
	contractHook := &requestContractHook{}
	h.registerBeforeRequestHook(contractHook)
	h.registerAfterSuccessHook(contractHook)
	// 3. Normalizes and parses Interactions API error responses
	interactionErrors := &interactionErrorHook{}
	h.registerAfterSuccessHook(interactionErrors)
	h.registerAfterErrorHook(interactionErrors)
}
