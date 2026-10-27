#!/usr/bin/env bash
# Step 0.6, part 2 (root, after reboot): isolation checks. Prints OK/FAIL for each.
set -u
A=$(id -u lv-a); B=$(id -u lv-b)
ok() { echo "OK   $*"; }; bad() { echo "FAIL $*"; }
check() { local want=$1 desc=$2; shift 2; if "$@" >/tmp/out.$$ 2>&1; then r=0; else r=1; fi
  if [[ $want == pass && $r == 0 ]] || [[ $want == fail && $r == 1 ]]; then ok "$desc"; else bad "$desc"; sed 's/^/     | /' /tmp/out.$$ | tail -5; fi; }
asuser() { local u=$1; shift; sudo -u $u env XDG_RUNTIME_DIR=/run/user/$(id -u $u) DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u $u)/bus "$@"; }

echo "== /proc"
mount | grep ' /proc ' 
check pass "logind is alive" loginctl list-users
check pass "user@A is active" systemctl is-active user@$A.service
check pass "systemctl --user for lv-a" asuser lv-a systemctl --user is-system-running --wait
check pass "systemd-run --user --scope for lv-a" asuser lv-a systemd-run --user --scope --quiet true
systemctl --failed --no-legend | sed 's/^/     failed: /'

echo "== processes"
asuser lv-b systemd-run --user --unit=bsleep --quiet sleep 600
sleep 1
check fail "lv-a cannot see lv-b processes" sh -c "sudo -u lv-a ps -eo user | grep -q lv-b"
check pass "ubuntu (procview) sees lv-b processes" sh -c "sudo -u ubuntu ps -eo user | grep -q lv-b"
check pass "ubuntu is in procview" sh -c "id ubuntu | grep -q procview"

echo "== files"
check fail "lv-a cannot read lv-b home" sudo -u lv-a ls /srv/laavu/agents/b
asuser lv-b systemd-run --user --scope --quiet sh -c 'echo secret > /tmp/b-secret'
ls -l /tmp/b-secret
check fail "lv-a cannot read lv-b /tmp file (umask from pam_umask/GECOS? session via systemd --user)" sudo -u lv-a cat /tmp/b-secret
rm -f /tmp/b-secret
sudo -u lv-b sh -c 'echo s > /tmp/b-secret2'; ls -l /tmp/b-secret2; rm -f /tmp/b-secret2
echo "     umask user@B: $(grep -i umask /proc/$(systemctl show -p MainPID --value user@$B.service)/status)"

echo "== ports (systemd-run --user --scope, user@.service inside user-UID.slice)"
pyserve() { echo "import socket,sys;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1',int(sys.argv[1])));s.listen();print('bound',s.getsockname())"; }
check pass "lv-a binds 20005" asuser lv-a systemd-run --user --scope --quiet python3 -c "$(pyserve)" 20005
check fail "lv-a cannot bind 30005" asuser lv-a systemd-run --user --scope --quiet python3 -c "$(pyserve)" 30005
check fail "lv-a cannot bind 21005 (lv-b range)" asuser lv-a systemd-run --user --scope --quiet python3 -c "$(pyserve)" 21005
check pass "lv-a: bind to port 0" asuser lv-a systemd-run --user --scope --quiet python3 -c "$(pyserve)" 0
check pass "lv-a: outbound curl" asuser lv-a systemd-run --user --scope --quiet curl -sS -m 10 -o /dev/null https://ubuntu.com
check pass "lv-a: outbound udp (DNS resolve)" asuser lv-a systemd-run --user --scope --quiet getent hosts ubuntu.com
echo "== ports via systemd-run --uid --slice (like check-isolation)"
check pass "slice: lv-a binds 20006" systemd-run --quiet --uid=lv-a --slice=user-$A.slice --pipe --wait python3 -c "$(pyserve)" 20006
check fail "slice: lv-a cannot bind 30006" systemd-run --quiet --uid=lv-a --slice=user-$A.slice --pipe --wait python3 -c "$(pyserve)" 30006
echo "== outside the slice (sudo -u without a user session), for comparison"
check fail "sudo -u lv-a without scope cannot bind 30007 (FAIL expected: the sudo process is outside the agent slice)" sudo -u lv-a python3 -c "$(pyserve)" 30007

echo "== nftables"
asuser lv-b systemd-run --user --unit=bsrv --quiet python3 -m http.server --bind 127.0.0.1 21005
sleep 2
check fail "lv-a cannot connect to lv-b 21005" asuser lv-a systemd-run --user --scope --quiet curl -sS -m 3 -o /dev/null http://127.0.0.1:21005/
check pass "lv-b connects to its own 21005" asuser lv-b systemd-run --user --scope --quiet curl -sS -m 3 -o /dev/null http://127.0.0.1:21005/
check pass "root connects to 21005" curl -sS -m 3 -o /dev/null http://127.0.0.1:21005/
check pass "ubuntu connects to 21005" sudo -u ubuntu curl -sS -m 3 -o /dev/null http://127.0.0.1:21005/
asuser lv-b systemctl --user stop bsrv bsleep

echo "== privilege escalation"
check fail "lv-a: sudo -n true" sudo -u lv-a sudo -n true
