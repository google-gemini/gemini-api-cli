// Command fakekeyring runs gemini-api against a file-backed keychain, so the
// contract tests never touch the OS keychain.
//
// FAKE_KEYRING_FILE (required) is the JSON file holding the entries.
// FAKE_KEYRING_UNAVAILABLE, when non-empty, makes every keychain call fail.
//
// Under testdata, this package is outside ./...; the contract tests build it.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/google-gemini/gemini-api-cli/internal/cli"
	"github.com/google-gemini/gemini-api-cli/internal/clierrors"
	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/zalando/go-keyring"
)

var errUnavailable = errors.New("fake keychain unavailable")

type fileKeyring struct {
	path        string
	unavailable bool
}

func (f fileKeyring) load() (map[string]string, error) {
	entries := map[string]string{}
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return entries, nil
	}
	return entries, json.Unmarshal(data, &entries)
}

func (f fileKeyring) save(entries map[string]string) error {
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return os.WriteFile(f.path, data, 0o600)
}

func (f fileKeyring) Get(service, key string) (string, error) {
	if f.unavailable {
		return "", errUnavailable
	}
	entries, err := f.load()
	if err != nil {
		return "", err
	}
	value, ok := entries[service+"/"+key]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}

func (f fileKeyring) Set(service, key, value string) error {
	if f.unavailable {
		return errUnavailable
	}
	entries, err := f.load()
	if err != nil {
		return err
	}
	entries[service+"/"+key] = value
	return f.save(entries)
}

func (f fileKeyring) Delete(service, key string) error {
	if f.unavailable {
		return errUnavailable
	}
	entries, err := f.load()
	if err != nil {
		return err
	}
	if _, ok := entries[service+"/"+key]; !ok {
		return keyring.ErrNotFound
	}
	delete(entries, service+"/"+key)
	return f.save(entries)
}

func main() {
	path := os.Getenv("FAKE_KEYRING_FILE")
	if path == "" {
		fmt.Fprintln(os.Stderr, "fakekeyring: FAKE_KEYRING_FILE is not set")
		os.Exit(1)
	}
	config.SetKeyringBackend(fileKeyring{path: path, unavailable: os.Getenv("FAKE_KEYRING_UNAVAILABLE") != ""})

	// Keep in sync with cmd/gemini-api/main.go.
	if err := cli.Execute(); err != nil {
		var rendered interface{ Rendered() bool }
		if !errors.As(err, &rendered) || !rendered.Rendered() {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(clierrors.ExitCode(err))
	}
}
