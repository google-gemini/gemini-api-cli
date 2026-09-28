package contract_test

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestOpenStdinPipeDoesNotStall: a caller that leaves stdin open without
// writing to it (a harness, a wrapper script) must not block a body-reading
// command, in agent mode or out of it. The generated pre-run bounds the read
// only in agent mode; custom.Register bounds it always.
func TestOpenStdinPipeDoesNotStall(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()

	args := append(plainArgs("https://example.invalid"), "--dry-run", "agent", "run", "hello", "--model", "gemini-3.6-flash")
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
	case <-done:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("command still blocked on an open stdin pipe after 15s\nstderr: %s", stderr.String())
	}
}
