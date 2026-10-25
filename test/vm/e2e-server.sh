#!/usr/bin/env bash
# Сквозная проверка сервера на чистой ВМ:
# откат к снапшоту clean → laavu install → два агента → check-isolation →
# сессии с заглушкой claude → перезапуск воркера → перезагрузка ВМ → спящие сессии.
#
#   test/vm/e2e-server.sh            полный прогон с откатом
#   NO_RESET=1 test/vm/e2e-server.sh без отката (ВМ уже с установкой)
source "$(dirname "$0")/lib.sh"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
export PATH="$HOME/.local/opt/go/bin:$PATH"

fail() { log "ПРОВАЛ: $*"; exit 1; }
ok() { log "ок: $*"; }

# Сборка под ВМ.
(cd "$ROOT" && CGO_ENABLED=0 go build -ldflags "-X main.Version=e2e" -o bin/ ./cmd/... && CGO_ENABLED=0 go build -o bin/fakeclaude ./test/fakeclaude) || fail "сборка"

if [[ -z "${NO_RESET:-}" ]]; then
  "$(dirname "$0")/reset.sh"
fi
scp -P "$VM_SSH_PORT" -i "$VM_SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
  "$ROOT/bin/laavu" "$ROOT/bin/laavud" "$ROOT/bin/fakeclaude" ubuntu@127.0.0.1:/tmp/ || fail "копирование"

R() { vm_ssh "sudo bash -c $(printf '%q' "$*")"; }
# call АГЕНТ ОП JSON — запрос к воркеру от laavud.
call() { R "runuser -u laavud -- laavud call $1 $2 '${3:-}'"; }
# opt АГЕНТ СЕССИЯ ОПЦИЯ — опция tmux-сессии агента.
opt() { R "sudo -u lv-$1 tmux -L laavu show-options -t '=$2:' -v @laavu-$3 2>/dev/null"; }
wait_for() { # описание, команда
  local what="$1"; shift
  for _ in $(seq 1 60); do
    if eval "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  fail "не дождались: $what"
}

log "установка"
out=$(R "cd /tmp && ./laavu install --agent demo") || { echo "$out"; fail "laavu install"; }
link=$(echo "$out" | grep -o 'https://localhost:8443/register#[A-Za-z0-9_-]*' | head -1)
[[ -n "$link" ]] || fail "нет ссылки регистрации"
ok "install, ссылка регистрации напечатана"
out=$(R "cd /tmp && ./laavu install --agent demo") || { echo "$out"; fail "повторный install"; }
echo "$out" | grep -q "register#" || fail "повторный install без ссылки, хотя администратора нет"
ok "повторный install ничего не ломает"

R "install -m755 /tmp/fakeclaude /usr/local/bin/fakeclaude"
R "laavu agent create research" >/dev/null || fail "второй агент"
R "sed -i 's|^claude = .*|claude = \"/usr/local/bin/fakeclaude\"|' /etc/laavu/agents/demo.toml && systemctl restart laavud-worker@lv-demo.service"
ok "агенты demo и research"

R "laavu check-isolation" || fail "check-isolation"
ok "check-isolation в обе стороны"
R "systemctl is-active laavud caddy laavu-nftables laavud-worker@lv-demo laavud-worker@lv-research" >/dev/null || fail "службы"

log "сессии"
call demo mkdir '{"name":"Demo"}' >/dev/null || fail "mkdir"
call demo mkdir '{"name":".ssh"}' >/dev/null 2>&1 && fail "зарезервированное имя прошло"
call demo new '{"folder":"Demo","kind":"claude","author":"alice","name":"Demo-1"}' >/dev/null || fail "новая сессия"
# Хуки из ~/.claude/settings.json агента → laavud hook → сокет демона → опции tmux.
wait_for "SessionStart через хуки" '[[ "$(opt demo Demo-1 status)" == waiting && -n "$(opt demo Demo-1 claude)" ]]'
first=$(opt demo Demo-1 claude)
ok "хук SessionStart дошёл до демона, UUID $first"
call demo input '{"name":"Demo-1","text":"привет\nвторая строка","enter":true}' >/dev/null || fail "ввод"
wait_for "ответ" 'call demo history "{\"name\":\"Demo-1\"}" | grep -q "echo: привет / вторая строка"'
ok "ввод из поля доходит одним сообщением"
call demo context '{"name":"Demo-1"}' | grep -q '"used"' || fail "контекст"
ok "счётчик контекста"

# tmux не в cgroup воркера: перезапуск воркера сессии не трогает.
R "systemctl restart laavud-worker@lv-demo.service"
sleep 2
R "sudo -u lv-demo tmux -L laavu has-session -t =Demo-1" || fail "сессия умерла с воркером"
cg=$(R 'for p in $(pgrep -u lv-demo tmux); do cat /proc/$p/cgroup; done')
[[ "$cg" == *user@1001.service* && "$cg" != *laavud-worker* ]] || fail "tmux не в user@1001.service: $cg"
ok "tmux живёт в пользовательском экземпляре systemd и переживает перезапуск воркера"

# umask сессии: файлы агента в /tmp закрыты.
call demo input '{"name":"Demo-1","text":"/exit","enter":true}' >/dev/null
call demo new '{"folder":"Demo","kind":"shell","name":"term"}' >/dev/null || fail "терминал"
call demo input '{"name":"term","text":"echo x > /tmp/laavu-umask-test; echo TMP=$TMPDIR PORTS=$LAAVU_PORTS","enter":true}' >/dev/null
wait_for "вывод терминала" 'call demo history "{\"name\":\"term\"}" | grep -q "PORTS=20000-20999"'
mode=$(R "stat -c %a /tmp/laavu-umask-test")
[[ "$mode" == 600 ]] || fail "файл агента в /tmp с правами $mode"
call demo history '{"name":"term"}' | grep -q "TMP=/srv/laavu/agents/demo/tmp" || fail "TMPDIR"
ok "umask 077, TMPDIR и LAAVU_PORTS в сессии"
wait_for "claude-сессия завершена" 'call demo sessions | grep -q "\"status\": \"exited\""'
ok "смерть процесса: статус exited"
call demo respawn '{"name":"Demo-1"}' >/dev/null || fail "respawn"
wait_for "resume после respawn" 'call demo history "{\"name\":\"Demo-1\"}" | grep -q "(resume)"'
ok "«продолжить» поднимает тот же разговор"

log "перезагрузка ВМ"
vm_ssh 'sudo systemctl reboot' || true
sleep 10
wait_ssh 300
wait_for "воркер после перезагрузки" 'R "systemctl is-active laavud-worker@lv-demo" | grep -qx active'
call demo sessions | grep -q '"sleeping": true' || fail "после перезагрузки нет спящих сессий"
ok "после перезагрузки сессии спят"
call demo ensure '{"name":"Demo-1"}' | grep -q '"started": true' || fail "ленивый подъём"
wait_for "resume после перезагрузки" 'call demo history "{\"name\":\"Demo-1\"}" | grep -q "(resume)"'
[[ "$(opt demo Demo-1 claude)" == "$first" ]] || fail "поднялся другой разговор"
ok "ленивый resume поднимает тот же разговор из той же папки"
R "laavu check-isolation" >/dev/null || fail "изоляция после перезагрузки"
R "nft list table inet laavu" | grep -q "dport 20000-20999" || fail "правила nftables не пережили перезагрузку"
findmnt_opts=$(R "findmnt -no OPTIONS /proc")
[[ "$findmnt_opts" == *hidepid=invisible* ]] || fail "hidepid после перезагрузки: $findmnt_opts"
ok "изоляция, nftables и hidepid после перезагрузки"

log "cleanup не ходит по симлинкам агента"
R "mkdir -p /root/victim && touch -d '30 days ago' /root/victim/keep && chmod 755 /root"
R "sudo -u lv-demo sh -c 'rm -rf ~/tmp && ln -s /root/victim ~/tmp'"
R "laavu cleanup" >/dev/null || true
R "test -f /root/victim/keep" || fail "cleanup удалил файл root по симлинку агента"
R "sudo -u lv-demo sh -c 'rm ~/tmp && mkdir ~/tmp && touch -d \"30 days ago\" ~/tmp/old && touch ~/tmp/new'; chmod 700 /root"
R "laavu cleanup" | grep -q "агент demo: удалено из ~/tmp: 1" || fail "cleanup не удалил старый файл агента"
R "test -f /srv/laavu/agents/demo/tmp/new && ! test -e /srv/laavu/agents/demo/tmp/old" || fail "cleanup удалил не то"
ok "cleanup от имени агента, по симлинку не ходит"

log "удаление агента"
R "laavu agent delete research" >/dev/null || fail "delete"
R "id lv-research" 2>/dev/null && fail "пользователь остался"
R "nft list table inet laavu" | grep -q 21000 && fail "правило удалённого агента осталось"
ok "агент удалён вместе с правилами"

log "браузер против ВМ"
if [[ -d "$ROOT/test/e2e/node_modules" ]]; then
  # Повторный install выдал вторую ссылку; первая тоже действует — берём последнюю.
  link=$(R "runuser -u laavud -- laavud invite --admin -q")
  (cd "$ROOT/test/e2e" && LAAVU_LINK="$link" npx playwright test -c playwright.vm.config.ts) || fail "браузерная проверка"
  ok "passkey, сессии, просмотр и dev-сервер в браузере через Caddy"
else
  log "пропуск: нет test/e2e/node_modules (npm ci в test/e2e)"
fi

log "ВСЁ ПРОШЛО"
