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

//go:build unix

package envruntime

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// ShutdownSignals returns the OS signals that trigger graceful runtime shutdown.
func ShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

// ConfigureSubprocessGroup places cmd in its own process group and configures
// cmd.Cancel to terminate the entire process group on context cancellation.
func ConfigureSubprocessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					return os.ErrProcessDone
				}
				return err
			}
		}
		return nil
	}
}
