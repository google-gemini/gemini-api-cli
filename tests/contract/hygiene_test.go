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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google-gemini/gemini-api-cli/internal/cli"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// This repository is published as open source. Help text, flag usage, and the
// comments and strings in its Go sources must not leak internal tool names,
// internal shortlinks, or monorepo paths. The terms are split so this file
// does not trip its own check.
var (
	internalTermPattern = regexp.MustCompile(`\b(` + strings.Join([]string{
		"One" + "Platform",
		"GOOGLE_" + "INTERNAL",
		"Stub" + "by",
		"stub" + "by",
		"Pi" + "per",
		"Criti" + "que",
		"Bo" + "rg",
		"Saw" + "mill",
		"Sher" + "log",
		"Uni" + "serve",
		"Bl" + "aze",
		"Ci" + "tC",
	}, "|") + `)\b`)
	internalLinkPattern = regexp.MustCompile(`(^|[\s("'\[]|https?://)((go|yaqs)/[a-zA-Z0-9_-]+|(b|cl)/[0-9]{5,})\b`)
	monorepoPathPattern = regexp.MustCompile(`\b` + "google" + `3/`)
)

// hygieneAllowlist names known findings, keyed by slash-separated path
// relative to the repository root, then by match. Every entry needs a reason.
var hygieneAllowlist = map[string][]string{
	// The envruntime import-allowlist test matches monorepo import paths,
	// which export rewrites to the public module path.
	"internal/cli/custom/envruntime/hygiene_test.go": {"google" + "3/"},
}

func hygieneFindings(text string) []string {
	var found []string
	for _, p := range []*regexp.Regexp{internalTermPattern, internalLinkPattern, monorepoPathPattern} {
		for _, m := range p.FindAllString(text, -1) {
			m = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(m), "http://"), "https://")
			m = strings.Trim(m, `("'[`)
			if goStdlibPackage[m] {
				continue
			}
			found = append(found, m)
		}
	}
	return found
}

// goStdlibPackage lists Go standard library import paths that look like
// shortlinks.
var goStdlibPackage = map[string]bool{
	"go/ast": true, "go/build": true, "go/constant": true, "go/doc": true,
	"go/format": true, "go/importer": true, "go/parser": true, "go/printer": true,
	"go/scanner": true, "go/token": true, "go/types": true, "go/version": true,
}

func allowed(rel, match string) bool {
	for _, a := range hygieneAllowlist[rel] {
		if a == match {
			return true
		}
	}
	return false
}

// TestExportHygieneSources scans the comments and string literals of every Go
// source, tests included (they are published too).
func TestExportHygieneSources(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "docs" || rel == "evals" || strings.HasPrefix(d.Name(), ".") && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			t.Errorf("parse %s: %v", rel, err)
			return nil
		}
		files++
		report := func(pos token.Pos, kind, text string) {
			for _, m := range hygieneFindings(text) {
				if !allowed(rel, m) {
					t.Errorf("%s: %s contains internal reference %q: %q", fset.Position(pos), kind, m, text)
				}
			}
		}
		imports := make(map[*ast.BasicLit]bool)
		for _, imp := range f.Imports {
			imports[imp.Path] = true
		}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				// Generated-file markers and directives are not prose.
				if strings.HasPrefix(c.Text, "//go:") {
					continue
				}
				report(c.Pos(), "comment", c.Text)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || imports[lit] {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil {
				report(lit.Pos(), "string literal", s)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 50 {
		t.Fatalf("scanned only %d Go files under %s; the sources are missing from the test data", files, root)
	}
}

// TestExportHygieneHelp scans what users read: every command's help text and
// every flag's usage string.
func TestExportHygieneHelp(t *testing.T) {
	isolateCLIEnv(t)
	root, err := cli.NewRootCommand()
	if err != nil {
		t.Fatalf("NewRootCommand() failed: %v", err)
	}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		path := cmd.CommandPath()
		for field, text := range map[string]string{"Short": cmd.Short, "Long": cmd.Long, "Example": cmd.Example} {
			for _, m := range hygieneFindings(text) {
				t.Errorf("%s %s contains internal reference %q:\n%s", path, field, m, text)
			}
		}
		check := func(f *pflag.Flag) {
			for _, m := range hygieneFindings(f.Usage) {
				t.Errorf("%s --%s usage contains internal reference %q: %q", path, f.Name, m, f.Usage)
			}
		}
		cmd.LocalNonPersistentFlags().VisitAll(check)
		cmd.PersistentFlags().VisitAll(check)
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

func TestHygienePatterns(t *testing.T) {
	for _, tc := range []struct {
		text string
		leak bool
	}{
		{"see " + "go/" + "some-link", true},
		{"fixed in " + "cl/" + "1018160594", true},
		{"tracked in " + "b/" + "585693029", true},
		{"see http://" + "b/" + "585693029", true},
		{"https://" + "go/" + "some-link", true},
		{"[" + "b/" + "585693029]", true},
		{"routed through " + "Uni" + "serve", true},
		{"speakeasy-sdk/go 0.4.6 " + "google" + "3/third_party/x", true},
		{"Run: gemini-api environments create", false},
		{"https://ai.google.dev/gemini-api/docs", false},
		{"go/types and a/b/c paths", false},
		{"input/output formats", false},
	} {
		if got := len(hygieneFindings(tc.text)) > 0; got != tc.leak {
			t.Errorf("hygieneFindings(%q) leak = %v, want %v", tc.text, got, tc.leak)
		}
	}
}
