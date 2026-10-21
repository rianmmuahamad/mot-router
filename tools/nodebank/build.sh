#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
export CARGO_BUILD_JOBS="${CARGO_BUILD_JOBS:-2}"
export GOMAXPROCS=2
mkdir -p dist
cargo build --locked --release
cp "${CARGO_TARGET_DIR:-target}/release/mot-nodebank" dist/mot-nodebank
(cd probe && go build -buildvcs=false -p 2 -trimpath -tags with_utls,with_grpc -ldflags='-s -w' -o ../dist/nodebank-probe .)
chmod 755 dist/mot-nodebank dist/nodebank-probe
