package contract_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// README examples are a contract: every `gemini-api ...` line inside a
// fenced bash block of README.md must be a real invocation. Each one runs as
// a black-box command with --dry-run appended (the request is built and
// previewed, nothing is sent), so a renamed command, a removed flag or a
// leftover `<placeholder>` fails this test instead of rotting in the docs.
//
// Skipped on purpose:
//   - lines that do not start with the CLI name (installers, curl, shell
//     control flow); a leading `KEY=value` environment assignment or an
//     `echo '<json>' |` stdin feed is understood and applied
//   - interactive / shell-integration commands: configure, auth, explore,
//     completion, help, and any example that passes --interactive
//   - blocks preceded by an HTML comment `<!-- readme-examples: skip ... -->`
//     (examples that need local files or a live service)
//   - invocations with input redirects, whose referenced files are unavailable
//
// Anything after an unquoted pipe or output redirect (`| jq`, `> out.json`) is
// not part of the invocation and is dropped.
func TestReadmeExamples(t *testing.T) {
	readmePath := findReadme(t)
	examples := collectReadmeExamples(t, readmePath)
	if len(examples) == 0 {
		t.Fatalf("no runnable README examples found in %s", readmePath)
	}

	for _, ex := range examples {
		ex := ex
		t.Run(fmt.Sprintf("line_%d", ex.line), func(t *testing.T) {
			args := append([]string{}, ex.args...)
			if !containsFlag(args, "--dry-run") {
				args = append(args, "--dry-run")
			}
			home := t.TempDir()
			var result commandResult
			if ex.stdin != "" {
				result = runCLIWithStdin(t, home, ex.env, ex.stdin, args...)
			} else {
				result = runCLI(t, home, ex.env, args...)
			}
			if result.err != nil {
				t.Fatalf("README.md:%d: %s\nerror: %v\nstderr:\n%s", ex.line, ex.raw, result.err, result.stderr)
			}
		})
	}

	t.Logf("exercised %d runnable README examples", len(examples))
}

type readmeExample struct {
	line  int
	raw   string
	args  []string
	env   map[string]string
	stdin string
}

func findReadme(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		candidate := filepath.Join(dir, "README.md")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("README.md not found near the test directory")
	return ""
}

var readmeSkippedCommands = map[string]bool{
	"configure":  true,
	"auth":       true,
	"explore":    true,
	"completion": true,
	"help":       true,
}

func collectReadmeExamples(t *testing.T, path string) []readmeExample {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var out []readmeExample
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	inBlock := false
	blockRunnable := false
	skipNext := false
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if !inBlock {
			if strings.HasPrefix(trimmed, "<!-- readme-examples: skip") {
				skipNext = true
				continue
			}
			if strings.HasPrefix(trimmed, "```") {
				lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				inBlock = true
				blockRunnable = (lang == "bash" || lang == "sh" || lang == "shell") && !skipNext
				skipNext = false
			} else if trimmed != "" {
				// A skip marker applies only to the block that follows it.
				skipNext = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			inBlock = false
			continue
		}
		if !blockRunnable || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		ex, ok := parseReadmeInvocation(trimmed)
		if !ok {
			continue
		}
		ex.line = lineNo
		ex.raw = trimmed
		out = append(out, ex)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// parseReadmeInvocation understands the shell subset README examples use:
// [KEY=value ...] [echo '<json>' |] gemini-api args... [| ... | > ...]
func parseReadmeInvocation(line string) (readmeExample, bool) {
	words, ok := shellWords(line)
	if !ok || len(words) == 0 {
		return readmeExample{}, false
	}
	ex := readmeExample{env: map[string]string{}}
	i := 0
	// `echo '<json>' | gemini-api ...` feeds stdin.
	if words[0].text == "echo" && !words[0].quoted {
		if len(words) >= 3 && words[2].op == "|" {
			ex.stdin = words[1].text
			i = 3
		} else {
			return readmeExample{}, false
		}
	}
	// Leading environment assignments. Quoting is allowed in the value, but
	// not in the name: FOO="value" is an assignment; "FOO"=value is not.
	for i < len(words) && !words[i].nameQuoted && words[i].op == "" {
		eq := strings.Index(words[i].text, "=")
		if eq <= 0 || strings.ContainsAny(words[i].text[:eq], "-/.") {
			break
		}
		ex.env[words[i].text[:eq]] = words[i].text[eq+1:]
		i++
	}
	if i >= len(words) || words[i].op != "" || words[i].text != "gemini-api" {
		return readmeExample{}, false
	}
	i++
	for ; i < len(words); i++ {
		if words[i].op != "" {
			if strings.HasPrefix(words[i].op, "<") {
				// Input redirects depend on local file contents this test cannot provide.
				return readmeExample{}, false
			}
			break // pipeline / redirect: the invocation ends here
		}
		ex.args = append(ex.args, words[i].text)
	}
	for _, a := range ex.args {
		if a == "--interactive" {
			// Explicit opt-in to prompts/forms: needs a terminal by design.
			return readmeExample{}, false
		}
	}
	for _, a := range ex.args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if readmeSkippedCommands[a] {
			return readmeExample{}, false
		}
		break
	}
	return ex, true
}

type shellWord struct {
	text       string
	quoted     bool   // any part of the word was quoted
	nameQuoted bool   // a quoted segment appeared before the first '='
	op         string // "|", "<", ">", "&&", ";" (control operator, unquoted)
}

// shellWords splits a single command line the way a POSIX shell tokenizes
// it (single quotes, double quotes, backslash escapes, and the control
// operators the examples use), without expanding anything.
func shellWords(line string) ([]shellWord, bool) {
	var words []shellWord
	var cur strings.Builder
	quoted := false
	nameQuoted := false
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, shellWord{text: cur.String(), quoted: quoted, nameQuoted: nameQuoted})
			cur.Reset()
			quoted = false
			nameQuoted = false
			inWord = false
		}
	}
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\'':
			if !strings.Contains(cur.String(), "=") {
				nameQuoted = true
			}
			inWord, quoted = true, true
			j := i + 1
			for j < len(runes) && runes[j] != '\'' {
				cur.WriteRune(runes[j])
				j++
			}
			if j >= len(runes) {
				return nil, false // unterminated
			}
			i = j
		case c == '"':
			if !strings.Contains(cur.String(), "=") {
				nameQuoted = true
			}
			inWord, quoted = true, true
			j := i + 1
			for j < len(runes) && runes[j] != '"' {
				if runes[j] == '\\' && j+1 < len(runes) {
					next := runes[j+1]
					if next == '$' || next == '`' || next == '"' || next == '\\' || next == '\n' {
						if next != '\n' {
							cur.WriteRune(next)
						}
						j += 2
						continue
					}
				}
				cur.WriteRune(runes[j])
				j++
			}
			if j >= len(runes) {
				return nil, false
			}
			i = j
		case c == '\\':
			if i+1 < len(runes) {
				inWord = true
				cur.WriteRune(runes[i+1])
				i++
			}
		case c == ' ' || c == '\t':
			flush()
		case c == '|' || c == '<' || c == '>' || c == ';' || c == '&':
			// `2>` / `2>&1` style redirects: the digit belongs to the operator.
			if inWord && !quoted && (c == '>' || c == '<') && cur.String() == "2" {
				cur.Reset()
				inWord = false
			}
			flush()
			op := string(c)
			for i+1 < len(runes) && (runes[i+1] == c || runes[i+1] == '&' || runes[i+1] == '>') {
				i++
				op += string(runes[i])
			}
			words = append(words, shellWord{op: op})
		default:
			inWord = true
			cur.WriteRune(c)
		}
	}
	flush()
	return words, true
}

func containsFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

func TestParseReadmeInvocationAllowsQuotedEnvironmentValue(t *testing.T) {
	ex, ok := parseReadmeInvocation(`CLI_QUERY_PARAM1="value with spaces" gemini-api version`)
	if !ok {
		t.Fatal("quoted environment value was not parsed")
	}
	if len(ex.env) != 1 || ex.env["CLI_QUERY_PARAM1"] != "value with spaces" {
		t.Errorf("env = %#v", ex.env)
	}
	if len(ex.args) != 1 || ex.args[0] != "version" {
		t.Errorf("args = %#v, want [version]", ex.args)
	}

	if _, ok := parseReadmeInvocation(`"CLI_QUERY_PARAM1"=value gemini-api version`); ok {
		t.Error("a quoted environment variable name was accepted as an assignment")
	}
}

func TestParseReadmeInvocationSkipsInputRedirect(t *testing.T) {
	if _, ok := parseReadmeInvocation(`gemini-api agent run < request.json`); ok {
		t.Fatal("input redirect was accepted without its file-backed stdin")
	}
}

func TestShellWordsPreservesNonSpecialBackslashInDoubleQuotes(t *testing.T) {
	words, ok := shellWords(`gemini-api --jq "\d+\$"`)
	if !ok {
		t.Fatal("shell words were not parsed")
	}
	if len(words) != 3 {
		t.Fatalf("word count = %d, want 3", len(words))
	}
	if words[2].text != `\d+$` {
		t.Errorf("jq word = %q, want %q", words[2].text, `\d+$`)
	}
}
