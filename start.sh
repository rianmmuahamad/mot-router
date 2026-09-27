#!/usr/bin/env bash
# Start Mot Router. Dashboard: http://127.0.0.1:8787
set -euo pipefail

cd "$(dirname "$0")"

# Build the binary if it is missing or older than the sources.
if [ ! -x ./mot-router ] || [ -n "$(find . -name '*.go' -newer ./mot-router 2>/dev/null)" ]; then
  echo "building mot-router..."
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o mot-router .
fi

# Optional: paid Zen models need a real credential.
# export OPENCODE_API_KEY=<zen-key>

exec ./mot-router "$@"
