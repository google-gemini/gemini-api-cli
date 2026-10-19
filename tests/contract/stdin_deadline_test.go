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

package contract_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOpenStdinPipeDoesNotStall: a caller that leaves stdin open without
// writing to it (a harness, a wrapper script) must not block a body-reading
// command, in agent mode or out of it. The generated pre-run bounds the read
// only in agent mode; custom.Register bounds it always. A request input given
// only through -i/--input or -f/--file counts as input supplied on the command
// line, so the silent pipe is skipped rather than reported as a timeout.
func TestOpenStdinPipeDoesNotStall(t *testing.T) {
	notes := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(notes, []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{
		{"agent", "run", "hello", "--model", "gemini-3.6-flash"},
		{"agent", "run", "-i", notes},
		{"generate", "-i", notes},
		{"image", "-i", notes},
		{"video", "-i", notes},
		{"music", "-f", notes},
	} {
		t.Run(strings.ReplaceAll(strings.Join(command, " "), notes, "notes.txt"), func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()

			args := append(append(plainArgs("https://example.invalid"), "--dry-run"), command...)
			cmd := exec.Command(cliBinary, args...)
			cmd.Env = isolatedEnv(t.TempDir(), nil)
			cmd.Stdin = reader
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("command failed on an open stdin pipe: %v\nstderr: %s", err, stderr.String())
				}
			case <-time.After(15 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("command still blocked on an open stdin pipe after 15s\nstderr: %s", stderr.String())
			}
			if !strings.Contains(stderr.String(), "[DRY-RUN] Would send: POST") {
				t.Fatalf("dry-run preview missing from stderr:\n%s", stderr.String())
			}
		})
	}
}
