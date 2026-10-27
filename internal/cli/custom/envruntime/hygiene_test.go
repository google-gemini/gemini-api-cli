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

package envruntime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func findPackageSourceDir(t *testing.T) string {
	t.Helper()
	const relPkg = "third_party/gemini_api_cli/internal/cli/custom/envruntime"
	candidates := []string{
		".",
		relPkg,
	}
	if srcDir := os.Getenv("TEST_SRCDIR"); srcDir != "" {
		ws := os.Getenv("TEST_WORKSPACE")
		if ws == "" {
			ws = "google3"
		}
		candidates = append(candidates, filepath.Join(srcDir, ws, relPkg))
	}
	for _, dir := range candidates {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
				return dir
			}
		}
	}
	t.Fatalf("could not locate package .go sources in any candidate directory: %v", candidates)
	return "."
}

func TestStdlibOnlyAndExportHygiene(t *testing.T) {
	pkgDir := findPackageSourceDir(t)
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%q) failed: %v", pkgDir, err)
	}
	fset := token.NewFileSet()
	bannedMutants := "NO" + "MUTANTS"
	bannedNolint := "//" + "nolint"
	bannedNolintUpper := "// " + "NOLINT"
	internalTermPattern := regexp.MustCompile(`\b(` + strings.Join([]string{
		"One" + "Platform",
		"GOOGLE_" + "INTERNAL",
		"E" + "SF",
		"Stub" + "by",
		"stub" + "by",
		"Pi" + "per",
		"Criti" + "que",
		"Bo" + "rg",
		"Span" + "ner",
		"Saw" + "mill",
		"Sher" + "log",
	}, "|") + `)\b`)
	internalLinkPattern := regexp.MustCompile(`(^|[\s(])(go/[a-zA-Z0-9_-]+|(b|cl)/[0-9]+)`)
	goFileCount := 0

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if name == "hooks.go" {
			t.Errorf("file %q is named hooks.go, which Speakeasy staging skips", name)
		}
		goFileCount++
		rawBytes, err := os.ReadFile(filepath.Join(pkgDir, name))
		if err != nil {
			t.Fatalf("os.ReadFile(%q) failed: %v", name, err)
		}
		src := string(rawBytes)
		if !strings.HasPrefix(src, "// Copyright 2026 Google LLC\n") {
			t.Errorf("file %q missing Apache 2.0 Copyright 2026 Google LLC header", name)
		}

		fileAST, err := parser.ParseFile(fset, name, rawBytes, parser.ParseComments)
		if err != nil {
			t.Fatalf("parser.ParseFile(%q) failed: %v", name, err)
		}
		importLiterals := make(map[*ast.BasicLit]bool, len(fileAST.Imports))
		for _, imp := range fileAST.Imports {
			importLiterals[imp.Path] = true
			impPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("strconv.Unquote(%s) failed: %v", imp.Path.Value, err)
			}
			firstElem, _, _ := strings.Cut(impPath, "/")
			if strings.HasPrefix(impPath, "google3/") || strings.Contains(firstElem, ".") {
				t.Errorf("file %q imports non-stdlib package %q, want Go standard library only", name, impPath)
			}
		}
		for _, cg := range fileAST.Comments {
			for _, c := range cg.List {
				if strings.Contains(c.Text, bannedMutants) || strings.Contains(c.Text, bannedNolint) || strings.Contains(c.Text, bannedNolintUpper) {
					t.Errorf("file %q contains disallowed directive comment %q", name, c.Text)
				}
				if match := internalTermPattern.FindString(c.Text); match != "" {
					t.Errorf("file %q comment contains Google-internal term %q: %q", name, match, c.Text)
				}
				if match := internalLinkPattern.FindString(c.Text); match != "" {
					t.Errorf("file %q comment contains Google-internal shortlink %q: %q", name, strings.TrimSpace(match), c.Text)
				}
			}
		}
		if name != "hygiene_test.go" {
			ast.Inspect(fileAST, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || importLiterals[lit] {
					return true
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				if match := internalTermPattern.FindString(val); match != "" {
					t.Errorf("file %q string literal contains Google-internal term %q: %q", name, match, val)
				}
				if match := internalLinkPattern.FindString(val); match != "" {
					t.Errorf("file %q string literal contains Google-internal shortlink %q: %q", name, strings.TrimSpace(match), val)
				}
				return true
			})
		}
	}
	if goFileCount == 0 {
		t.Fatal("TestStdlibOnlyAndExportHygiene found 0 .go files")
	}
}
