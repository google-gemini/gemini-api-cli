#!/usr/bin/env bash
# Build both CLIs under test and install shims into bin/<harness>/.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$here/../.." && pwd)
reference_dir="$repo_root/reference/gemini-api-cli-poc"

# reference/ is gitignored — clone it when absent (fresh checkout / CI).
if [[ ! -d "$reference_dir" ]]; then
  echo "Cloning reference CLI..."
  git clone --depth 1 https://github.com/LyalinDotCom/gemini-api-cli-poc "$reference_dir"
fi

mkdir -p "$here/bin/generated" "$here/bin/reference"

echo "Building generated CLI (gemini-api)..."
(cd "$repo_root" && go build -o "$here/bin/generated/gemini-api-real" ./cmd/gemini-api)

# Antigravity's command runner gives child processes an open stdin pipe even
# when the command has no input. Keep that harness detail out of the CLI under
# test. An explicit --body @- still opts into reading the caller's stdin.
cat > "$here/bin/generated/gemini-api" <<EOF
#!/usr/bin/env bash
for arg in "\$@"; do
  if [[ "\$arg" == "@-" || "\$arg" == *=@- ]]; then
    exec "$here/bin/generated/gemini-api-real" "\$@"
  fi
done
exec "$here/bin/generated/gemini-api-real" "\$@" </dev/null
EOF
chmod +x "$here/bin/generated/gemini-api"

if [[ ! -f "$reference_dir/dist/cli.js" ]]; then
  echo "Building reference CLI (gemini-api-cli)..."
  (cd "$reference_dir" && npm install --silent && npm run build)
fi

if [[ ! -x "$here/.venv/bin/python3" ]]; then
  echo "Creating Python venv for the Antigravity driver..."
  python3 -m venv "$here/.venv"
fi
"$here/.venv/bin/pip" install --quiet google-antigravity

cat > "$here/bin/reference/gemini-api-cli" <<EOF
#!/usr/bin/env bash
exec node "$reference_dir/dist/cli.js" "\$@"
EOF
chmod +x "$here/bin/reference/gemini-api-cli"

echo "Setup complete."
echo "  bin/generated/gemini-api"
echo "  bin/reference/gemini-api-cli"
