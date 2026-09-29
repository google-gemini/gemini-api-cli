#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

if ! unformatted=$(find cmd internal tests -name '*.go' -type f -print0 | xargs -0 gofmt -l); then
	echo 'Failed to run gofmt validation.' >&2
	exit 1
fi

# Temperarily disable gofmt validation until we figure out if we want to format
# the generated code.
# if [[ -n "$unformatted" ]]; then
#   printf 'Go files need gofmt:\n%s\n' "$unformatted" >&2
#   exit 1
# fi

go vet -structtag=false ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 \
  -checks=inherit,-SA1019,-SA5008,-S1040,-U1000 ./...
go test ./... -count=1
go build ./...

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT
GOBIN="$tmp_dir/bin" go install ./cmd/gemini-api
"$tmp_dir/bin/gemini-api" --help >/dev/null
"$tmp_dir/bin/gemini-api" --usage >"$tmp_dir/usage.kdl"
grep -q 'name "gemini-api"' "$tmp_dir/usage.kdl"

echo 'Non-live validation passed.'
