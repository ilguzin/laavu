#!/usr/bin/env bash
# Установка с ноутбука и обновления:
# свежая ВМ с паролем → laavu install --ssh с подписанным релизом → вход только
# по ключу → laavu update на новую версию → битый релиз откатывается.
source "$(dirname "$0")/lib.sh"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
export PATH="$HOME/.local/opt/go/bin:$PATH"
fail() { log "ПРОВАЛ: $*"; exit 1; }
ok() { log "ок: $*"; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export XDG_CONFIG_HOME="$WORK/config"

# Ключ проекта для теста и два релиза: 0.0.1 и 0.0.2.
"$ROOT/bin/laavu" release keygen --out "$WORK/release.key" >"$WORK/keygen.txt" 2>/dev/null || (cd "$ROOT" && go build -o bin/ ./cmd/laavu && "$ROOT/bin/laavu" release keygen --out "$WORK/release.key" >"$WORK/keygen.txt")
PUB=$(tail -1 "$WORK/keygen.txt")
build_release() { # версия каталог [битый]
  local v=$1 dir=$2
  mkdir -p "$dir"
  local ld="-X main.Version=$v -X github.com/ilguzin/laavu/internal/release.ProjectKey=$PUB"
  (cd "$ROOT" && CGO_ENABLED=0 go build -ldflags "$ld" -o "$dir/laavu-linux-amd64" ./cmd/laavu) || fail "сборка laavu $v"
  if [[ -n "${3:-}" ]]; then
    printf '#!/bin/sh\n[ "$1" = version ] && echo %s && exit 0\nexit 1\n' "$v" >"$dir/laavud-linux-amd64"
    chmod 755 "$dir/laavud-linux-amd64"
  else
    (cd "$ROOT" && CGO_ENABLED=0 go build -ldflags "$ld" -o "$dir/laavud-linux-amd64" ./cmd/laavud) || fail "сборка laavud $v"
  fi
  "$ROOT/bin/laavu" release manifest --dir "$dir" --version "$v"
  "$ROOT/bin/laavu" release sign --dir "$dir" --key "$WORK/release.key"
}
build_release 0.0.1 "$WORK/r1"
build_release 0.0.2 "$WORK/r2"
build_release 0.0.3 "$WORK/r3" broken
# laavu на «ноутбуке» — из релиза 0.0.1, с вшитым ключом проекта.
cp "$WORK/r1/laavu-linux-amd64" "$WORK/laavu"
ssh-keygen -q -t ed25519 -N '' -C owner-test -f "$WORK/owner"

[[ -z "${NO_RESET:-}" ]] && "$(dirname "$0")/reset.sh"

log "laavu install --ssh по паролю"
out=$(printf '%s\n' "$VM_PASSWORD" | "$WORK/laavu" install --ssh "ubuntu@127.0.0.1:$VM_SSH_PORT" --identity none --yes \
  --release "$WORK/r1" --owner-pubkey "$WORK/owner.pub" --agent demo 2>&1) || { echo "$out"; fail "install --ssh"; }
echo "$out" | grep -q "Отпечаток ключа сервера: SHA256:" || fail "нет отпечатка хоста"
echo "$out" | grep -q "Релиз 0.0.1: подпись проверена" || fail "нет проверки подписи на ноутбуке"
echo "$out" | grep -q "подпись и файлы сходятся" || fail "нет проверки подписи на сервере"
link=$(echo "$out" | grep -o 'https://localhost:8443/register#[A-Za-z0-9_-]*' | head -1)
[[ -n "$link" ]] || { echo "$out"; fail "нет ссылки регистрации"; }
echo "$link" >"$ROOT/test/vm/.last-register-link"
echo "$out" | grep -q "только по ключу" || { echo "$out"; fail "пароль не запрещён"; }
ok "установка с ноутбука, ссылка: $link"

ssh -p "$VM_SSH_PORT" -i "$WORK/owner" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR ubuntu@127.0.0.1 true || fail "вход по ключу владельца"
if sshpass -V >/dev/null 2>&1; then
  sshpass -p "$VM_PASSWORD" ssh -p "$VM_SSH_PORT" -o PubkeyAuthentication=no -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@127.0.0.1 true 2>/dev/null && fail "вход по паролю остался"
fi
vm_ssh 'sudo sshd -T | grep -qx "passwordauthentication no"' || fail "PasswordAuthentication не выключен"
vm_ssh "sudo cat /etc/laavu/release.pub" | grep -qx "$PUB" || fail "ключ проекта не закреплён"
[[ "$(vm_ssh 'laavud version')" == 0.0.1 ]] || fail "версия после установки"
ok "вход только по ключу, ключ проекта закреплён"

# Повторное подключение с тем же ключом хоста — без вопросов; подменённый — отказ.
sed -i 's/SHA256:[A-Za-z0-9+\/]*/SHA256:AAAAподмена/' "$XDG_CONFIG_HOME/laavu/known_hosts"
out=$(printf '\n' | "$WORK/laavu" install --ssh "ubuntu@127.0.0.1:$VM_SSH_PORT" --identity "$WORK/owner" --release "$WORK/r1" 2>&1) && fail "подменённый ключ хоста принят"
echo "$out" | grep -q "изменился" || { echo "$out"; fail "нет сообщения о смене ключа хоста"; }
ok "смена ключа хоста ловится"

log "laavu update"
scp -P "$VM_SSH_PORT" -i "$VM_SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -r "$WORK/r2" "$WORK/r3" ubuntu@127.0.0.1:/tmp/ || fail "копирование релизов"
vm_ssh 'sudo laavu update laavud --from /tmp/r2' || fail "update 0.0.2"
[[ "$(vm_ssh 'laavud version')" == 0.0.2 ]] || fail "версия после update"
ok "обновление до 0.0.2"
vm_ssh 'sudo laavu update laavud --from /tmp/r3 --timeout 8s' && fail "битый релиз встал"
[[ "$(vm_ssh 'laavud version')" == 0.0.2 ]] || fail "откат не вернул 0.0.2"
vm_ssh 'systemctl is-active laavud' | grep -qx active || fail "laavud после отката"
ok "битый релиз откатился на 0.0.2"
# Подпись чужим ключом.
"$ROOT/bin/laavu" release keygen --out "$WORK/evil.key" >/dev/null
"$ROOT/bin/laavu" release sign --dir "$WORK/r2" --key "$WORK/evil.key"
scp -P "$VM_SSH_PORT" -i "$VM_SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "$WORK/r2/manifest.json.sig" ubuntu@127.0.0.1:/tmp/r2/ || fail "копирование"
out=$(vm_ssh 'sudo laavu update laavud --from /tmp/r2 --force' 2>&1) && fail "чужая подпись принята"
echo "$out" | grep -q "подпись релиза не сходится" || fail "нет сообщения о подписи: $out"
ok "релиз с чужой подписью отклонён"
log "ВСЁ ПРОШЛО"
