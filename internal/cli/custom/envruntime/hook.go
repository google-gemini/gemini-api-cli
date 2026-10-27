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

package envruntime

import "context"

// HookHandler serves CallHookRequest events over an interaction stream.
type HookHandler interface {
	CallHook(ctx context.Context, req *CallHookRequest) (*CallHookResponse, error)
}

// Observer receives lifecycle telemetry notifications from workers and pollers.
type Observer interface {
	LeasePolled(leased int, err error)
	WorkerProvisioned(reused bool, err error)
	HandoffFinished(outcome string)
	InteractionStarted(interactionID string)
	InteractionFinished(interactionID string, err error)
}

// NopObserver is an Observer whose methods are all no-ops.
type NopObserver struct{}

// LeasePolled implements Observer.
func (NopObserver) LeasePolled(int, error) {}

// WorkerProvisioned implements Observer.
func (NopObserver) WorkerProvisioned(bool, error) {}

// HandoffFinished implements Observer.
func (NopObserver) HandoffFinished(string) {}

// InteractionStarted implements Observer.
func (NopObserver) InteractionStarted(string) {}

// InteractionFinished implements Observer.
func (NopObserver) InteractionFinished(string, error) {}
