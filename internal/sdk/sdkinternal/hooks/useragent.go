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

package hooks

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/google-gemini/gemini-api-cli/internal/sdk/sdkinternal/config"
)

const defaultFallbackVersion = "dev"

// UserAgent formats the CLI User-Agent string following the google-genai
// client convention: {product}/{version} gl-{lang}/{runtime},
// e.g. "google-genai-cli/0.5.4 gl-go/go1.24.0".
func UserAgent(version string) string {
	v := strings.TrimSpace(version)
	if !isValidVersionToken(v) {
		v = defaultFallbackVersion
	}
	return fmt.Sprintf("google-genai-cli/%s gl-go/%s", v, runtime.Version())
}

// ResolveVersion returns the effective CLI version for User-Agent headers,
// preferring a build-time "-X main.version=..." linker override or a clean
// module release tag over fallback.
func ResolveVersion(fallback string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return sanitizeFallbackVersion(fallback)
	}
	return resolveVersionFromBuildInfo(fallback, info)
}

func sanitizeFallbackVersion(fallback string) string {
	v := strings.TrimSpace(fallback)
	if !isValidVersionToken(v) {
		return defaultFallbackVersion
	}
	return v
}

func resolveVersionFromBuildInfo(fallback string, info *debug.BuildInfo) string {
	cleanFallback := sanitizeFallbackVersion(fallback)
	if info == nil || strings.HasSuffix(info.Path, ".test") {
		return cleanFallback
	}
	var ldVersion string
	for _, setting := range info.Settings {
		if setting.Key != "-ldflags" {
			continue
		}
		rawFlags, ok := extractApplicableLDFlags(setting.Value, info.Path)
		if !ok {
			continue
		}
		tokens := splitLDFlags(rawFlags)
		for i := 0; i < len(tokens); i++ {
			tok := tokens[i]
			var kv string
			switch {
			case (tok == "-X" || tok == "--X") && i+1 < len(tokens):
				i++
				kv = tokens[i]
			case strings.HasPrefix(tok, "-X="):
				kv = strings.TrimPrefix(tok, "-X=")
			case strings.HasPrefix(tok, "--X="):
				kv = strings.TrimPrefix(tok, "--X=")
			default:
				continue
			}
			if k, v, ok := strings.Cut(kv, "="); ok && k == "main.version" {
				ldVersion = strings.TrimSpace(v)
			}
		}
	}
	if isValidVersionToken(ldVersion) {
		return ldVersion
	}
	if !hasVCSSetting(info.Settings) {
		if modVer := strings.TrimSpace(info.Main.Version); isCleanModuleVersion(modVer) {
			if len(modVer) >= 2 && (modVer[0] == 'v' || modVer[0] == 'V') && modVer[1] >= '0' && modVer[1] <= '9' {
				return modVer[1:]
			}
			return modVer
		}
	}
	return cleanFallback
}

// isValidVersionToken reports whether s is a non-empty RFC 9110 product-version
// token (1*tchar), rejecting whitespace, '/', HTTP delimiters, and control bytes.
func isValidVersionToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		isAlphaNum := (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
		if isAlphaNum {
			continue
		}
		switch ch {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func extractApplicableLDFlags(raw, binPath string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	if trimmed[0] == '-' || trimmed[0] == '\'' || trimmed[0] == '"' {
		return trimmed, true
	}
	pat, rest, hasEq := strings.Cut(trimmed, "=")
	if !hasEq {
		return trimmed, true
	}
	pat = strings.TrimSpace(pat)
	if pat == "all" || pat == "main" || pat == "." || pat == "./..." || pat == "..." ||
		pat == binPath || strings.HasSuffix(pat, "/cmd/gemini-api") ||
		(strings.HasSuffix(pat, "/...") && strings.HasPrefix(binPath, strings.TrimSuffix(pat, "..."))) {
		return rest, true
	}
	return "", false
}

// splitLDFlags tokenizes a Go -ldflags argument string using the same quoting
// rules as cmd/internal/quoted.Split (single/double quotes, no backslash
// escaping so Windows paths with backslashes are preserved verbatim).
func splitLDFlags(s string) []string {
	var tokens []string
	for len(s) > 0 {
		for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\r' || s[0] == '\n') {
			s = s[1:]
		}
		if len(s) == 0 {
			break
		}
		var cur strings.Builder
		var quote byte
		for len(s) > 0 {
			ch := s[0]
			if quote != 0 {
				s = s[1:]
				if ch == quote {
					quote = 0
					continue
				}
				cur.WriteByte(ch)
				continue
			}
			if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' {
				break
			}
			if ch == '\'' || ch == '"' {
				quote = ch
				s = s[1:]
				continue
			}
			cur.WriteByte(ch)
			s = s[1:]
		}
		tokens = append(tokens, cur.String())
	}
	return tokens
}

func hasVCSSetting(settings []debug.BuildSetting) bool {
	for _, s := range settings {
		if s.Key == "vcs" || strings.HasPrefix(s.Key, "vcs.") {
			return true
		}
	}
	return false
}

func isCleanModuleVersion(v string) bool {
	if v == "" || v == "(devel)" || strings.Contains(v, "+") || isPseudoVersion(v) {
		return false
	}
	if len(v) >= 2 && (v[0] == 'v' || v[0] == 'V') && v[1] >= '0' && v[1] <= '9' {
		v = v[1:]
	}
	base, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if !isAllASCIIDigits(p) {
			return false
		}
	}
	if hasPre && !isValidSemVerPrerelease(pre) {
		return false
	}
	return true
}

func isValidSemVerPrerelease(pre string) bool {
	if pre == "" {
		return false
	}
	for _, ident := range strings.Split(pre, ".") {
		if ident == "" {
			return false
		}
		for i := 0; i < len(ident); i++ {
			ch := ident[i]
			isDigit := ch >= '0' && ch <= '9'
			isLower := ch >= 'a' && ch <= 'z'
			isUpper := ch >= 'A' && ch <= 'Z'
			if !isDigit && !isLower && !isUpper && ch != '-' {
				return false
			}
		}
	}
	return true
}

func isPseudoVersion(v string) bool {
	base, _, _ := strings.Cut(v, "+")
	_, pre, ok := strings.Cut(base, "-")
	if !ok {
		return false
	}
	parts := strings.Split(pre, "-")
	if len(parts) < 2 {
		return false
	}
	tsPart := parts[len(parts)-2]
	if idx := strings.LastIndexByte(tsPart, '.'); idx >= 0 {
		tsPart = tsPart[idx+1:]
	}
	revPart := parts[len(parts)-1]
	return len(tsPart) == 14 && isAllASCIIDigits(tsPart) && len(revPart) >= 12 && isAllASCIIHex(revPart)
}

func isAllASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isAllASCIIHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		isDigit := ch >= '0' && ch <= '9'
		isLower := ch >= 'a' && ch <= 'f'
		isUpper := ch >= 'A' && ch <= 'F'
		if !isDigit && !isLower && !isUpper {
			return false
		}
	}
	return true
}

// userAgentHook rewrites the default Speakeasy User-Agent into the
// google-genai client convention.
type userAgentHook struct{}

func (userAgentHook) SDKInit(cfg config.SDKConfiguration) config.SDKConfiguration {
	cfg.UserAgent = UserAgent(ResolveVersion(cfg.SDKVersion))
	return cfg
}
