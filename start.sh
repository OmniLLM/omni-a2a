#!/usr/bin/env bash
# Quick-start: build (if needed) and start the Omni A2A Gateway daemon.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$SCRIPT_DIR/oah"

# Build if binary doesn't exist.
if [ ! -f "$BINARY" ]; then
    echo "Building oah..."
    (cd "$SCRIPT_DIR" && go build -o oah ./cmd/omni-agent-hub/)
fi

# Forward all arguments (e.g. start, stop, restart, status, serve).
# Default to "start" if no argument given.
ACTION="${1:-start}"
shift 2>/dev/null || true

exec "$BINARY" "$ACTION" "$@"
