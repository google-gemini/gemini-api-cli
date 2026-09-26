package contract_test

import (
	"strings"
	"testing"
)

// The Build Spec's per-command help template: ≤ ~40 lines, runnable examples
// first, defaults shown, global flags behind one --help-global pointer, and an
// identical machine-interface footer on every command.

const (
	helpMachineLine = "Machine interface: --output-format json · --jq <expr> · --dry-run · --usage"
	helpGlobalsLine = "Globals (auth, network, output): gemini-api --help-global"
)

func helpLines(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// TestHelpBudgetAndFooter pins the compact help contract on the tier-1
// intents, the porcelain, an operation leaf, and a group command.
func TestHelpBudgetAndFooter(t *testing.T) {
	commands := [][]string{
		{"generate"}, {"image"}, {"video"}, {"music"},
		{"tts"}, {"tokens"}, {"analyze"}, {"transcribe"},
		{"agent"}, {"agent", "run"}, {"agent", "status"},
		{"models"}, {"models", "list"}, {"files"}, {"files", "upload"},
	}
	for _, args := range commands {
		name := strings.Join(args, " ")
		t.Run(name, func(t *testing.T) {
			result := runCLI(t, t.TempDir(), nil, append(args, "--help")...)
			if result.err != nil {
				t.Fatalf("%s --help failed: %v\nstderr: %s", name, result.err, result.stderr)
			}
			out := result.stdout
			if n := helpLines(out); n > 40 {
				t.Errorf("%s --help is %d lines, want ≤ 40:\n%s", name, n, out)
			}
			for _, want := range []string{helpMachineLine, helpGlobalsLine} {
				if !strings.Contains(out, want) {
					t.Errorf("%s --help missing footer line %q:\n%s", name, want, out)
				}
			}
			for _, reject := range []string{"Diagnostics:", "Global Flags:", "Authentication:", "--api-key string"} {
				if strings.Contains(out, reject) {
					t.Errorf("%s --help still dumps global flags (%q):\n%s", name, reject, out)
				}
			}
		})
	}
}

// TestHelpDefaultsAndEscalation pins the "Defaults:" line and the
// Learn/escalate footer for a generated intent and a hand-written porcelain
// command, so both authoring surfaces render through the same template.
func TestHelpDefaultsAndEscalation(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"generate"}, []string{
			"Just works:",
			"Explain concurrency in one sentence",
			"Defaults: model gemini-3.6-flash · streams the reply (--stream=false for one result)",
			"Learn: https://ai.google.dev/gemini-api/docs/text-generation · escalate: full request control via gemini-api agent run",
		}},
		{[]string{"image"}, []string{
			"Defaults: model gemini-3.1-flash-image · output ./gemini-image-{timestamp}-{rand}.{ext}",
			"Learn: https://ai.google.dev/gemini-api/docs/image-generation · escalate: full request control via gemini-api agent run",
		}},
		{[]string{"tts"}, []string{
			"Just works:\n  gemini-api tts \"Welcome to the show\"",
			"Defaults: model gemini-3.1-flash-tts-preview · voice Kore · 24kHz mono WAV",
			"Learn: https://ai.google.dev/gemini-api/docs/speech-generation · escalate: full request control via gemini-api agent run",
		}},
		{[]string{"files", "upload"}, []string{
			"Defaults: MIME type detected from the extension",
			"Learn: https://ai.google.dev/gemini-api/docs/files · escalate: metadata-only registration via gemini-api files register",
		}},
	}
	for _, tc := range cases {
		name := strings.Join(tc.args, " ")
		result := runCLI(t, t.TempDir(), nil, append(tc.args, "--help")...)
		if result.err != nil {
			t.Fatalf("%s --help failed: %v\nstderr: %s", name, result.err, result.stderr)
		}
		for _, want := range tc.want {
			if !strings.Contains(result.stdout, want) {
				t.Errorf("%s --help missing %q:\n%s", name, want, result.stdout)
			}
		}
		if strings.Count(result.stdout, "Machine interface:") != 1 {
			t.Errorf("%s --help must carry exactly one machine-interface line:\n%s", name, result.stdout)
		}
	}
}

// TestHelpGlobal pins the root-only --help-global surface: the grouped global
// flags print once, keyless and before config validation, and subcommands do
// not accept the flag.
func TestHelpGlobal(t *testing.T) {
	result := runCLI(t, t.TempDir(), nil, "--help-global")
	if result.err != nil {
		t.Fatalf("--help-global failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if !strings.HasPrefix(result.stdout, "Global flags (apply to every command):\n") {
		t.Errorf("--help-global does not start with the global-flags banner:\n%s", result.stdout)
	}
	for _, want := range []string{"Output:", "Authentication:", "API Parameters:", "Network:", "Diagnostics:", "--api-key string", "--server-url string", "--api-version string", "--usage"} {
		if !strings.Contains(result.stdout, want) {
			t.Errorf("--help-global missing %q:\n%s", want, result.stdout)
		}
	}

	sub := runCLI(t, t.TempDir(), nil, "generate", "--help-global")
	if sub.err == nil {
		t.Fatalf("generate --help-global unexpectedly succeeded:\n%s", sub.stdout)
	}
	if !strings.Contains(sub.stderr, "unknown flag: --help-global") {
		t.Errorf("generate --help-global should be an unknown flag, got:\n%s", sub.stderr)
	}

	usage := runCLI(t, t.TempDir(), nil, "--usage")
	if !strings.Contains(usage.stdout, `flag "--help-global"`) {
		t.Errorf("root --usage does not document --help-global:\n%s", usage.stdout)
	}
}

// TestRootHelpLayout pins the Build Spec's normative root structure: grouped
// commands, then the machine interface, the globals pointer and the setup line
// — no global flag dump on the root page.
func TestRootHelpLayout(t *testing.T) {
	result := runCLI(t, t.TempDir(), nil, "--help")
	if result.err != nil {
		t.Fatalf("--help failed: %v\nstderr: %s", result.err, result.stderr)
	}
	out := result.stdout
	for _, want := range []string{
		"Create:", "Understand:", "Manage:", "Advanced:",
		helpMachineLine, helpGlobalsLine,
		"Setup: export GEMINI_API_KEY=...   or   gemini-api configure",
		`Use "gemini-api [command] --help" for more information about a command.`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("root help missing %q:\n%s", want, out)
		}
	}
	for _, reject := range []string{"Diagnostics:", "Authentication:", "--api-key string", "--help-global  "} {
		if strings.Contains(out, reject) {
			t.Errorf("root help unexpectedly contains %q:\n%s", reject, out)
		}
	}
	// The footer order is Machine interface → Globals → Setup.
	mi, gl, su := strings.Index(out, helpMachineLine), strings.Index(out, helpGlobalsLine), strings.Index(out, "Setup: export")
	if !(mi < gl && gl < su) {
		t.Errorf("root footer order wrong (machine=%d globals=%d setup=%d):\n%s", mi, gl, su, out)
	}
}

// TestPorcelainHelpCopy pins the user-facing wording of the hand-written
// commands: stdout contracts are qualified ("By default"), since
// --output-format and --jq replace them, and help names no command that does
// not exist.
func TestPorcelainHelpCopy(t *testing.T) {
	t.Run("files", func(t *testing.T) {
		result := runCLI(t, t.TempDir(), nil, "files", "--help")
		if result.err != nil {
			t.Fatalf("files --help failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if strings.Contains(strings.ToLower(result.stdout), "download") {
			t.Errorf("files --help advertises a download command that does not exist:\n%s", result.stdout)
		}
	})
	for _, args := range [][]string{{"tts"}, {"analyze"}, {"transcribe"}, {"files", "upload"}} {
		name := strings.Join(args, " ")
		t.Run(name, func(t *testing.T) {
			result := runCLI(t, t.TempDir(), nil, append(args, "--help")...)
			if result.err != nil {
				t.Fatalf("%s --help failed: %v\nstderr: %s", name, result.err, result.stderr)
			}
			if strings.Contains(result.stdout, "stdout carries only") {
				t.Errorf("%s --help states an unqualified stdout contract:\n%s", name, result.stdout)
			}
			if !strings.Contains(result.stdout, "By default, stdout") {
				t.Errorf("%s --help does not qualify its stdout contract:\n%s", name, result.stdout)
			}
		})
	}
	// The identifier is required once, as an argument or a flag: the flag alone
	// must not claim to be required next to a "[file]" usage line.
	for _, args := range [][]string{{"files", "get"}, {"files", "delete"}, {"models", "get"}} {
		name := strings.Join(args, " ")
		t.Run(name+" flag is not marked required", func(t *testing.T) {
			result := runCLI(t, t.TempDir(), nil, append(args, "--help")...)
			if strings.Contains(result.stdout, "[required]") {
				t.Errorf("%s --help marks the flag form required:\n%s", name, result.stdout)
			}
		})
	}
	// Limits and rewrites the commands apply on their own must be disclosed,
	// worded as this CLI's behavior rather than a service rule.
	for name, wants := range map[string][]string{
		"analyze":      {"this CLI keeps each request under 20 MB", "gemini-api files upload"},
		"transcribe":   {"this CLI keeps each request under 20 MB", "alias: captions", "one input only"},
		"tts":          {"--out foo.mp3 is written as foo.wav", "An existing file is replaced", "gemini-tts-<unix-ms>-<random>"},
		"files upload": {"2 GB per file", "(default 5m)", "<path>"},
	} {
		t.Run(name+" disclosures", func(t *testing.T) {
			result := runCLI(t, t.TempDir(), nil, append(strings.Fields(name), "--help")...)
			for _, want := range wants {
				if !strings.Contains(result.stdout, want) {
					t.Errorf("%s --help missing %q:\n%s", name, want, result.stdout)
				}
			}
			if strings.Contains(result.stdout, "must go through") {
				t.Errorf("%s --help words a CLI limit as a service rule:\n%s", name, result.stdout)
			}
		})
	}
}
