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

//go:build windows

package custom

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func runtimeShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

func configureSubprocessGroup(cmd *exec.Cmd) {
	if len(cmd.Args) == 3 && strings.EqualFold(cmd.Args[1], "/C") {
		base := strings.ToLower(filepath.Base(cmd.Args[0]))
		if base == "cmd.exe" || base == "cmd" {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				CmdLine: `cmd.exe /d /s /c "` + cmd.Args[2] + `"`,
			}
		}
	}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
