#!/usr/bin/env sh
set -eu
PROJECT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$PROJECT_DIR"
export GOCACHE="$PROJECT_DIR/.cache/go-build"
exec go run ./webui/server.go
