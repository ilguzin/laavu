#!/bin/sh
# Поднимает laavud dev для e2e: свой дом агента, свой каталог сокетов tmux,
# заглушка claude вместо настоящего. Вывод — в .server-<порт>.log (там ссылка регистрации).
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
