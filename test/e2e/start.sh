#!/bin/sh
# Starts laavud dev for e2e: its own agent home, its own tmux socket directory,
# a claude stub instead of the real one. Output goes to .server-<port>.log (the registration link is there).
set -e
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
PORT=$1
shift
STATE="$ROOT/test/e2e/.state-$PORT"
rm -rf "$STATE"
mkdir -p "$STATE/home/.claude" "$STATE/tmux"
sed "s#{{.Laavud}}#$ROOT/bin/laavud#g" "$ROOT/deploy/agent/settings.json.tmpl" > "$STATE/home/.claude/settings.json"
export TMUX_TMPDIR="$STATE/tmux"
exec "$ROOT/bin/laavud" dev --home "$STATE/home" --claude "$ROOT/bin/fakeclaude" --port "$PORT" "$@" > "$ROOT/test/e2e/.server-$PORT.log" 2>&1
