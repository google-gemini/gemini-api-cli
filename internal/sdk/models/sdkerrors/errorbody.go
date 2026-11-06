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

// Injected by scripts/sync_speakeasy_outputs.py DO NOT EDIT.

package sdkerrors

import "encoding/json"

// defaultMessage is the message Speakeasy passes to NewSDKDefaultError when it has
// nothing more specific to report.
const defaultMessage = "API error occurred"

// messageFromBody replaces defaultMessage with the error.message of a JSON
// error body, when the body has one.
func messageFromBody(message, body string) string {
	if message != defaultMessage || body == "" {
		return message
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &parsed) != nil || parsed.Error.Message == "" {
		return message
	}
	return parsed.Error.Message
}
