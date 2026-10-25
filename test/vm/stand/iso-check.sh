#!/usr/bin/env bash
# Шаг 0.6, часть 2 (root, после перезагрузки): проверки изоляции. Печатает OK/FAIL по каждой.
set -u
A=$(id -u lv-a); B=$(id -u lv-b)
ok() { echo "OK   $*"; }; bad() { echo "FAIL $*"; }
check() { local want=$1 desc=$2; shift 2; if "$@" >/tmp/out.$$ 2>&1; then r=0; else r=1; fi
  if [[ $want == pass && $r == 0 ]] || [[ $want == fail && $r == 1 ]]; then ok "$desc"; else bad "$desc"; sed 's/^/     | /' /tmp/out.$$ | tail -5; fi; }
asuser() { local u=$1; shift; sudo -u $u env XDG_RUNTIME_DIR=/run/user/$(id -u $u) DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u $u)/bus "$@"; }

echo "== /proc"
mount | grep ' /proc ' 
check pass "logind жив" loginctl list-users
check pass "user@A активен" systemctl is-active user@$A.service
check pass "systemctl --user у lv-a" asuser lv-a systemctl --user is-system-running --wait
check pass "systemd-run --user --scope у lv-a" asuser lv-a systemd-run --user --scope --quiet true
systemctl --failed --no-legend | sed 's/^/     failed: /'

echo "== процессы"
asuser lv-b systemd-run --user --unit=bsleep --quiet sleep 600
sleep 1
check fail "lv-a не видит процессы lv-b" sh -c "sudo -u lv-a ps -eo user | grep -q lv-b"
check pass "ubuntu (procview) видит процессы lv-b" sh -c "sudo -u ubuntu ps -eo user | grep -q lv-b"
check pass "ubuntu в procview" sh -c "id ubuntu | grep -q procview"

echo "== файлы"
check fail "lv-a не читает дом lv-b" sudo -u lv-a ls /srv/laavu/agents/b
asuser lv-b systemd-run --user --scope --quiet sh -c 'echo secret > /tmp/b-secret'
ls -l /tmp/b-secret
check fail "lv-a не читает /tmp lv-b (umask из pam_umask/GECOS? сессия через systemd --user)" sudo -u lv-a cat /tmp/b-secret
rm -f /tmp/b-secret
sudo -u lv-b sh -c 'echo s > /tmp/b-secret2'; ls -l /tmp/b-secret2; rm -f /tmp/b-secret2
echo "     umask user@B: $(grep -i umask /proc/$(systemctl show -p MainPID --value user@$B.service)/status)"

echo "== порты (systemd-run --user --scope, user@.service внутри user-UID.slice)"
pyserve() { echo "import socket,sys;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1',int(sys.argv[1])));s.listen();print('bound',s.getsockname())"; }
check pass "lv-a биндит 20005" asuser lv-a systemd-run --user --scope --quiet python3 -c "$(pyserve)" 20005
check fail "lv-a не биндит 30005" asuser lv-a systemd-run --user --scope --quiet python3 -c "$(pyserve)" 30005
check fail "lv-a не биндит 21005 (диапазон lv-b)" asuser lv-a systemd-run --user --scope --quiet python3 -c "$(pyserve)" 21005
check pass "lv-a: bind на порт 0" asuser lv-a systemd-run --user --scope --quiet python3 -c "$(pyserve)" 0
check pass "lv-a: исходящий curl наружу" asuser lv-a systemd-run --user --scope --quiet curl -sS -m 10 -o /dev/null https://ubuntu.com
check pass "lv-a: udp наружу (DNS резолв)" asuser lv-a systemd-run --user --scope --quiet getent hosts ubuntu.com
echo "== порты через systemd-run --uid --slice (как check-isolation)"
check pass "slice: lv-a биндит 20006" systemd-run --quiet --uid=lv-a --slice=user-$A.slice --pipe --wait python3 -c "$(pyserve)" 20006
check fail "slice: lv-a не биндит 30006" systemd-run --quiet --uid=lv-a --slice=user-$A.slice --pipe --wait python3 -c "$(pyserve)" 30006
echo "== вне слайса (sudo -u без user-сессии) — для сравнения"
check fail "sudo -u lv-a без scope не биндит 30007 (ожидаемо FAIL: sudo-процесс вне слайса агента)" sudo -u lv-a python3 -c "$(pyserve)" 30007

echo "== nftables"
asuser lv-b systemd-run --user --unit=bsrv --quiet python3 -m http.server --bind 127.0.0.1 21005
sleep 2
check fail "lv-a не подключается к 21005 lv-b" asuser lv-a systemd-run --user --scope --quiet curl -sS -m 3 -o /dev/null http://127.0.0.1:21005/
check pass "lv-b подключается к своему 21005" asuser lv-b systemd-run --user --scope --quiet curl -sS -m 3 -o /dev/null http://127.0.0.1:21005/
check pass "root подключается к 21005" curl -sS -m 3 -o /dev/null http://127.0.0.1:21005/
check pass "ubuntu подключается к 21005" sudo -u ubuntu curl -sS -m 3 -o /dev/null http://127.0.0.1:21005/
asuser lv-b systemctl --user stop bsrv bsleep

echo "== повышение прав"
check fail "lv-a: sudo -n true" sudo -u lv-a sudo -n true
