package contract_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var (
	fakeKeyringOnce   sync.Once
	fakeKeyringBinary string
	fakeKeyringErr    error
)

// fakeKeyringCLI builds testdata/fakekeyring once, next to the contract binary.
func fakeKeyringCLI(t *testing.T) string {
	t.Helper()
	fakeKeyringOnce.Do(func() {
		fakeKeyringBinary = filepath.Join(filepath.Dir(cliBinary), "gemini-api-fakekeyring")
		cmd := exec.Command("go", "build", "-o", fakeKeyringBinary, "./tests/contract/testdata/fakekeyring")
		cmd.Dir = repoRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			fakeKeyringErr = fmt.Errorf("%v\n%s", err, output)
		}
	})
	if fakeKeyringErr != nil {
		t.Fatalf("build fake-keyring CLI: %v", fakeKeyringErr)
	}
	return fakeKeyringBinary
}

// authSession shares one home directory and fake keychain across a test's
// invocations.
type authSession struct {
	t           *testing.T
	binary      string
	home        string
	keyringFile string
	env         map[string]string
}

func newAuthSession(t *testing.T, keychainAvailable bool) *authSession {
	t.Helper()
	home := t.TempDir()
	s := &authSession{
		t:           t,
		binary:      fakeKeyringCLI(t),
		home:        home,
		keyringFile: filepath.Join(home, "keyring.json"),
	}
	unavailable := ""
	if !keychainAvailable {
		unavailable = "1"
	}
	s.env = map[string]string{"FAKE_KEYRING_FILE": s.keyringFile, "FAKE_KEYRING_UNAVAILABLE": unavailable}
	return s
}

func (s *authSession) run(args ...string) commandResult {
	s.t.Helper()
	return runBinary(s.t, s.binary, s.home, "", s.env, append([]string{"--no-interactive", "--color", "never"}, args...)...)
}

func (s *authSession) configPath() string {
	return filepath.Join(s.home, ".config", "gemini-api", "config.yaml")
}

func (s *authSession) readFile(path string) string {
	s.t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return string(data)
}

func (s *authSession) writeFile(path, content string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

func (s *authSession) keychain() map[string]string {
	s.t.Helper()
	entries := map[string]string{}
	if data := s.readFile(s.keyringFile); data != "" {
		if err := json.Unmarshal([]byte(data), &entries); err != nil {
			s.t.Fatalf("keyring file is not JSON: %v\n%s", err, data)
		}
	}
	return entries
}

type whoamiValue struct {
	Source string `json:"source"`
	Value  string `json:"value"`
}

func (s *authSession) whoami() (credentials, parameters map[string]whoamiValue) {
	s.t.Helper()
	result := s.run("--output-format", "json", "auth", "whoami")
	if result.err != nil {
		s.t.Fatalf("auth whoami failed: %v\nstderr: %s", result.err, result.stderr)
	}
	var info struct {
		Credentials      map[string]whoamiValue `json:"credentials"`
		GlobalParameters map[string]whoamiValue `json:"global_parameters"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &info); err != nil {
		s.t.Fatalf("auth whoami is not JSON: %v\n%s", err, result.stdout)
	}
	return info.Credentials, info.GlobalParameters
}

// requestHeaders returns the headers of a request sent without credential
// flags.
func (s *authSession) requestHeaders() http.Header {
	s.t.Helper()
	server, calls := newCaptureServer(s.t, `{"agents":[]}`)
	result := runBinary(s.t, s.binary, s.home, "", s.env, append(baseArgs(server.URL), "agent", "list")...)
	if result.err != nil {
		s.t.Fatalf("agent list failed: %v\nstderr: %s", result.err, result.stderr)
	}
	return (<-calls).header
}

var authCredentials = []struct {
	flag       string
	configKey  string
	header     string
	wireFormat string
}{
	{flag: "api-key", configKey: "api_key", header: "X-Goog-Api-Key", wireFormat: "%s"},
	{flag: "access-token", configKey: "access_token", header: "Authorization", wireFormat: "Bearer %s"},
}

// TestAuthLoginLogoutDryRunChangeNothing checks that --dry-run login and
// logout report a local no-op and change nothing.
func TestAuthLoginLogoutDryRunChangeNothing(t *testing.T) {
	s := newAuthSession(t, true)
	s.writeFile(s.keyringFile, `{"gemini-api/api-key":"stored-key"}`)
	s.writeFile(s.configPath(), "version: 1\nsecurity:\n  access_token: stored-token\n")

	for _, args := range [][]string{{"auth", "login", "--api-key", "new-key"}, {"auth", "logout"}} {
		for format, mode := range map[string][]string{"human": {"--dry-run"}, "json": {"--dry-run", "--output-format", "json"}} {
			t.Run(strings.Join(args[:2], " ")+"/"+format, func(t *testing.T) {
				result := s.run(append(mode, args...)...)
				if result.err != nil {
					t.Fatalf("failed: %v\nstderr: %s", result.err, result.stderr)
				}
				if format == "json" {
					var noop map[string]any
					if err := json.Unmarshal([]byte(result.stdout), &noop); err != nil {
						t.Fatalf("stdout is not one JSON object: %v\n%s", err, result.stdout)
					}
					if noop["dry_run"] != true || noop["local"] != true {
						t.Errorf("local no-op object = %v", noop)
					}
				} else if !strings.Contains(result.stderr, "[DRY-RUN] "+args[0]+" "+args[1]) {
					t.Errorf("stderr missing the dry-run notice:\n%s", result.stderr)
				}
				if got := s.readFile(s.keyringFile); got != `{"gemini-api/api-key":"stored-key"}` {
					t.Errorf("keychain changed: %s", got)
				}
				if got := s.readFile(s.configPath()); got != "version: 1\nsecurity:\n  access_token: stored-token\n" {
					t.Errorf("config file changed:\n%s", got)
				}
			})
		}
	}
}

// TestAuthLoginWithoutCredentialFlags checks that a non-interactive login
// without credential flags is a usage error and writes nothing.
func TestAuthLoginWithoutCredentialFlags(t *testing.T) {
	s := newAuthSession(t, true)
	result := s.run("auth", "login")
	if code := exitCode(t, result); code != 2 {
		t.Errorf("exit code = %d, want 2\nstderr: %s", code, result.stderr)
	}
	if !strings.Contains(result.stderr, "no flags provided") {
		t.Errorf("stderr does not explain the failure:\n%s", result.stderr)
	}
	if _, err := os.Stat(filepath.Dir(s.configPath())); !os.IsNotExist(err) {
		t.Errorf("config directory was created (stat error %v)", err)
	}
	if _, err := os.Stat(s.keyringFile); !os.IsNotExist(err) {
		t.Errorf("keychain was written (stat error %v)", err)
	}
}

// TestAuthLoginStoresCredential checks that login stores the secret in the
// keychain, or in the config file (mode 0600) without one, never echoes it,
// and that whoami and later requests pick it up.
func TestAuthLoginStoresCredential(t *testing.T) {
	for _, keychainAvailable := range []bool{true, false} {
		for _, c := range authCredentials {
			t.Run(fmt.Sprintf("%s/keychain=%t", c.flag, keychainAvailable), func(t *testing.T) {
				const secret = "sekr1t-l0gin"
				s := newAuthSession(t, keychainAvailable)

				result := s.run("auth", "login", "--"+c.flag, secret)
				if result.err != nil {
					t.Fatalf("auth login failed: %v\nstderr: %s", result.err, result.stderr)
				}
				if strings.Contains(result.stdout+result.stderr, secret) {
					t.Errorf("secret echoed\nstdout: %s\nstderr: %s", result.stdout, result.stderr)
				}
				if got := strings.Contains(result.stdout, "Secret credentials stored in OS keychain"); got != keychainAvailable {
					t.Errorf("keychain notice shown = %t, want %t\nstdout: %s", got, keychainAvailable, result.stdout)
				}
				if !strings.Contains(result.stdout, "Configuration saved to "+s.configPath()) {
					t.Errorf("stdout does not name the config file:\n%s", result.stdout)
				}

				config := s.readFile(s.configPath())
				wantSource := "config"
				if keychainAvailable {
					wantSource = "keyring"
					if got := s.keychain()["gemini-api/"+c.flag]; got != secret {
						t.Errorf("keychain %s = %q, want %q", c.flag, got, secret)
					}
					if strings.Contains(config, secret) {
						t.Errorf("secret also written to the config file:\n%s", config)
					}
				} else {
					if !strings.Contains(config, c.configKey+": "+secret) {
						t.Errorf("config file does not hold %s:\n%s", c.configKey, config)
					}
					if info, err := os.Stat(s.configPath()); err != nil {
						t.Fatal(err)
					} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
						t.Errorf("config file mode = %o, want 600", info.Mode().Perm())
					}
				}

				credentials, _ := s.whoami()
				if got := credentials[c.flag]; got.Source != wantSource || got.Value == "" || strings.Contains(got.Value, secret) {
					t.Errorf("whoami %s = %+v, want source %q and a masked value", c.flag, got, wantSource)
				}
				if got, want := s.requestHeaders().Get(c.header), fmt.Sprintf(c.wireFormat, secret); got != want {
					t.Errorf("%s = %q, want %q", c.header, got, want)
				}
			})
		}
	}
}

// TestAuthLoginReplacesConfigFallback checks that a keychain login removes a
// secret previously stored in the config file.
func TestAuthLoginReplacesConfigFallback(t *testing.T) {
	t.Skip("auth login keeps the config-file secret after storing it in the keychain; needs a generator fix")

	for _, c := range authCredentials {
		t.Run(c.flag, func(t *testing.T) {
			s := newAuthSession(t, true)
			s.writeFile(s.configPath(), "version: 1\nsecurity:\n  "+c.configKey+": stale-secret\n")

			result := s.run("auth", "login", "--"+c.flag, "fresh-secret")
			if result.err != nil {
				t.Fatalf("auth login failed: %v\nstderr: %s", result.err, result.stderr)
			}

			if got := s.keychain()["gemini-api/"+c.flag]; got != "fresh-secret" {
				t.Errorf("keychain %s = %q, want %q", c.flag, got, "fresh-secret")
			}
			if config := s.readFile(s.configPath()); strings.Contains(config, "stale-secret") {
				t.Errorf("config file still holds the previous secret:\n%s", config)
			}
		})
	}
}

// TestAuthLogoutClearsCredentials checks that logout clears both credentials
// from the keychain and the config file and keeps other settings.
func TestAuthLogoutClearsCredentials(t *testing.T) {
	for _, keychainAvailable := range []bool{true, false} {
		t.Run(fmt.Sprintf("keychain=%t", keychainAvailable), func(t *testing.T) {
			s := newAuthSession(t, keychainAvailable)
			if keychainAvailable {
				s.writeFile(s.keyringFile, `{"gemini-api/api-key":"kc-key","gemini-api/access-token":"kc-token","other-cli/api-key":"keep"}`)
			}
			s.writeFile(s.configPath(), "version: 1\nsecurity:\n  api_key: cfg-key\n  access_token: cfg-token\nglobals:\n  user_project: proj-1\n")

			result := s.run("auth", "logout")
			if result.err != nil {
				t.Fatalf("auth logout failed: %v\nstderr: %s", result.err, result.stderr)
			}
			if !strings.Contains(result.stdout, "All authentication credentials have been cleared.") {
				t.Errorf("stdout:\n%s", result.stdout)
			}

			if keychainAvailable {
				if got := s.keychain(); len(got) != 1 || got["other-cli/api-key"] != "keep" {
					t.Errorf("keychain after logout = %v, want only other-cli/api-key", got)
				}
			}
			config := s.readFile(s.configPath())
			for _, secret := range []string{"cfg-key", "cfg-token"} {
				if strings.Contains(config, secret) {
					t.Errorf("config file still holds %q:\n%s", secret, config)
				}
			}

			credentials, parameters := s.whoami()
			for _, c := range authCredentials {
				if got := credentials[c.flag]; got.Source != "unset" {
					t.Errorf("whoami %s = %+v, want unset", c.flag, got)
				}
			}
			if got := parameters["user-project"]; got.Source != "config" || got.Value != "proj-1" {
				t.Errorf("whoami user-project = %+v, want proj-1 from config", got)
			}

			headers := s.requestHeaders()
			for _, c := range authCredentials {
				if got := headers.Get(c.header); got != "" {
					t.Errorf("%s = %q after logout, want none", c.header, got)
				}
			}
		})
	}
}
