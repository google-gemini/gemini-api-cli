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

package envruntime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ShutdownSignals returns the OS signals that trigger graceful runtime shutdown.
func ShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

// ConfigureSubprocessGroup places cmd in a new Windows process group, preserves
// cmd.exe /C quoting, and terminates child process trees on cancellation.
func ConfigureSubprocessGroup(cmd *exec.Cmd) {
	sysAttr := &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	if len(cmd.Args) == 3 && strings.EqualFold(cmd.Args[1], "/C") {
		base := strings.ToLower(filepath.Base(cmd.Args[0]))
		if base == "cmd.exe" || base == "cmd" {
			sysAttr.CmdLine = `cmd.exe /d /s /c "` + cmd.Args[2] + `"`
		}
	}
	cmd.SysProcAttr = sysAttr
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if cmd.Process.Pid > 0 {
			taskkillPath := "taskkill.exe"
			if sysRoot := strings.TrimSpace(os.Getenv("SystemRoot")); sysRoot != "" {
				taskkillPath = filepath.Join(sysRoot, "System32", "taskkill.exe")
			}
			killCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := exec.CommandContext(killCtx, taskkillPath, "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
			cancel()
			if err == nil {
				return nil
			}
		}
		return cmd.Process.Kill()
	}
}
