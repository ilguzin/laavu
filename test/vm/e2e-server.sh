#!/usr/bin/env bash
# End-to-end server test on a clean VM:
# restore the clean snapshot → laavu install → two agents → check-isolation →
# sessions with the claude stub → worker restart → VM reboot → sleeping sessions.
#
#   test/vm/e2e-server.sh            full run with restore
#   NO_RESET=1 test/vm/e2e-server.sh no restore (VM already has an install)
source "$(dirname "$0")/lib.sh"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
export PATH="$HOME/.local/opt/go/bin:$PATH"

fail() { log "FAIL: $*"; exit 1; }
ok() { log "ok: $*"; }

# Build for the VM.
(cd "$ROOT" && CGO_ENABLED=0 go build -ldflags "-X main.Version=e2e" -o bin/ ./cmd/... && CGO_ENABLED=0 go build -o bin/fakeclaude ./test/fakeclaude) || fail "build"

if [[ -z "${NO_RESET:-}" ]]; then
  "$(dirname "$0")/reset.sh"
fi
scp -P "$VM_SSH_PORT" -i "$VM_SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
  "$ROOT/bin/laavu" "$ROOT/bin/laavud" "$ROOT/bin/fakeclaude" ubuntu@127.0.0.1:/tmp/ || fail "copy"

R() { vm_ssh "sudo bash -c $(printf '%q' "$*")"; }
# call AGENT OP JSON — a request to the worker on behalf of laavud.
call() { R "runuser -u laavud -- laavud call $1 $2 '${3:-}'"; }
# opt AGENT SESSION OPTION — a tmux session option of the agent.
opt() { R "sudo -u lv-$1 tmux -L laavu show-options -t '=$2:' -v @laavu-$3 2>/dev/null"; }
wait_for() { # description, command
  local what="$1"; shift
  for _ in $(seq 1 60); do
    if eval "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  fail "timed out waiting for: $what"
}

log "install"
out=$(R "cd /tmp && ./laavu install --host localhost --agent demo") || { echo "$out"; fail "laavu install"; }
link=$(echo "$out" | grep -o 'https://localhost:8443/register#[A-Za-z0-9_-]*' | head -1)
[[ -n "$link" ]] || fail "no registration link"
ok "install, registration link printed"
out=$(R "cd /tmp && ./laavu install --agent demo") || { echo "$out"; fail "repeated install"; }
echo "$out" | grep -q "register#" || fail "repeated install printed no link although there is no admin"
ok "repeated install breaks nothing"

R "install -m755 /tmp/fakeclaude /usr/local/bin/fakeclaude"
R "laavu agent create research" >/dev/null || fail "second agent"
R "sed -i 's|^claude = .*|claude = \"/usr/local/bin/fakeclaude\"|' /etc/laavu/agents/demo.toml && systemctl restart laavud-worker@lv-demo.service"
ok "agents demo and research"

R "laavu check-isolation" || fail "check-isolation"
ok "check-isolation in both directions"
R "systemctl is-active laavud caddy laavu-nftables laavud-worker@lv-demo laavud-worker@lv-research" >/dev/null || fail "services"

log "sessions"
call demo mkdir '{"name":"Demo"}' >/dev/null || fail "mkdir"
call demo mkdir '{"name":".ssh"}' >/dev/null 2>&1 && fail "reserved name was accepted"
call demo new '{"folder":"Demo","kind":"claude","author":"alice","name":"Demo-1"}' >/dev/null || fail "new session"
# Hooks from the agent's ~/.claude/settings.json → laavud hook → daemon socket → tmux options.
wait_for "SessionStart via hooks" '[[ "$(opt demo Demo-1 status)" == waiting && -n "$(opt demo Demo-1 claude)" ]]'
first=$(opt demo Demo-1 claude)
ok "SessionStart hook reached the daemon, UUID $first"
call demo input '{"name":"Demo-1","text":"héllo\nsecond line","enter":true}' >/dev/null || fail "input"
wait_for "reply" 'call demo history "{\"name\":\"Demo-1\"}" | grep -q "echo: héllo / second line"'
ok "input from the text field arrives as one message"
call demo context '{"name":"Demo-1"}' | grep -q '"used"' || fail "context"
ok "context counter"

# tmux is not in the worker's cgroup: restarting the worker leaves sessions alone.
R "systemctl restart laavud-worker@lv-demo.service"
sleep 2
R "sudo -u lv-demo tmux -L laavu has-session -t =Demo-1" || fail "session died with the worker"
cg=$(R 'for p in $(pgrep -u lv-demo tmux); do cat /proc/$p/cgroup; done')
[[ "$cg" == *user@1001.service* && "$cg" != *laavud-worker* ]] || fail "tmux is not in user@1001.service: $cg"
ok "tmux lives in the user systemd instance and survives a worker restart"

# Session umask: agent files in /tmp are private.
call demo input '{"name":"Demo-1","text":"/exit","enter":true}' >/dev/null
call demo new '{"folder":"Demo","kind":"shell","name":"term"}' >/dev/null || fail "terminal"
call demo input '{"name":"term","text":"echo x > /tmp/laavu-umask-test; echo TMP=$TMPDIR PORTS=$LAAVU_PORTS","enter":true}' >/dev/null
wait_for "terminal output" 'call demo history "{\"name\":\"term\"}" | grep -q "PORTS=20000-20999"'
mode=$(R "stat -c %a /tmp/laavu-umask-test")
[[ "$mode" == 600 ]] || fail "agent file in /tmp has mode $mode"
call demo history '{"name":"term"}' | grep -q "TMP=/srv/laavu/agents/demo/tmp" || fail "TMPDIR"
ok "umask 077, TMPDIR and LAAVU_PORTS in the session"
wait_for "claude session exited" 'call demo sessions | grep -q "\"status\": \"exited\""'
ok "process death: status exited"
call demo respawn '{"name":"Demo-1"}' >/dev/null || fail "respawn"
wait_for "resume after respawn" 'call demo history "{\"name\":\"Demo-1\"}" | grep -q "(resume)"'
ok "\"continue\" brings back the same conversation"

log "VM reboot"
vm_ssh 'sudo systemctl reboot' || true
sleep 10
wait_ssh 300
wait_for "worker after reboot" 'R "systemctl is-active laavud-worker@lv-demo" | grep -qx active'
call demo sessions | grep -q '"sleeping": true' || fail "no sleeping sessions after reboot"
ok "sessions are sleeping after reboot"
call demo ensure '{"name":"Demo-1"}' | grep -q '"started": true' || fail "lazy start"
wait_for "resume after reboot" 'call demo history "{\"name\":\"Demo-1\"}" | grep -q "(resume)"'
[[ "$(opt demo Demo-1 claude)" == "$first" ]] || fail "a different conversation came back"
ok "lazy resume brings back the same conversation from the same folder"
R "laavu check-isolation" >/dev/null || fail "isolation after reboot"
R "nft list table inet laavu" | grep -q "dport 20000-20999" || fail "nftables rules did not survive the reboot"
findmnt_opts=$(R "findmnt -no OPTIONS /proc")
[[ "$findmnt_opts" == *hidepid=invisible* ]] || fail "hidepid after reboot: $findmnt_opts"
ok "isolation, nftables and hidepid after reboot"

log "cleanup does not follow agent symlinks"
R "mkdir -p /root/victim && touch -d '30 days ago' /root/victim/keep && chmod 755 /root"
R "sudo -u lv-demo sh -c 'rm -rf ~/tmp && ln -s /root/victim ~/tmp'"
R "laavu cleanup" >/dev/null || true
R "test -f /root/victim/keep" || fail "cleanup deleted a root file through an agent symlink"
R "sudo -u lv-demo sh -c 'rm ~/tmp && mkdir ~/tmp && touch -d \"30 days ago\" ~/tmp/old && touch ~/tmp/new'; chmod 700 /root"
R "laavu cleanup" | grep -q "agent demo: removed from ~/tmp: 1" || fail "cleanup did not delete the old agent file"
R "test -f /srv/laavu/agents/demo/tmp/new && ! test -e /srv/laavu/agents/demo/tmp/old" || fail "cleanup deleted the wrong files"
ok "cleanup runs as the agent and does not follow symlinks"

log "agent deletion"
R "laavu agent delete research" >/dev/null || fail "delete"
R "id lv-research" 2>/dev/null && fail "user still exists"
R "nft list table inet laavu" | grep -q 21000 && fail "rule of the deleted agent is still there"
ok "agent deleted along with its rules"

log "browser against the VM"
if [[ -d "$ROOT/test/e2e/node_modules" ]]; then
  # The repeated install issued a second link; the first one is valid too, take the latest.
  link=$(R "runuser -u laavud -- laavud invite --admin -q")
  (cd "$ROOT/test/e2e" && LAAVU_LINK="$link" npx playwright test -c playwright.vm.config.ts) || fail "browser test"
  ok "passkey, sessions, preview and dev server in the browser via Caddy"
else
  log "skipped: no test/e2e/node_modules (npm ci in test/e2e)"
fi

log "ALL PASSED"
