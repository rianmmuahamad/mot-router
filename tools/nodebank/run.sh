#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
umask 077
exec ./dist/mot-nodebank --data "${NODEBANK_DATA:-data}" serve --worker ./dist/nodebank-probe "$@"
