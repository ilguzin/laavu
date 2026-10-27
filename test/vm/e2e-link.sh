#!/usr/bin/env bash
# Console link against the gateway stub on a fresh VM: binding by a pending code,
# report without agent names, jobs by rights, the helper's checks, rights,
# unlink from the console, binding by an issued code, laavu unlink.
#
# Uses its own VM so the everyday one is not reset:
#   VM_NAME=laavu-link VM_SSH_PORT=2232 VM_FORWARD_PORTS= test/vm/create.sh
#   VM_NAME=laavu-link VM_SSH_PORT=2232 test/vm/e2e-link.sh
source "$(dirname "$0")/lib.sh"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
export PATH="$HOME/.local/opt/go/bin:$PATH"
fail() { log "FAIL: $*"; exit 1; }
ok() { log "ok: $*"; }

(cd "$ROOT" && CGO_ENABLED=0 go build -o bin/ ./cmd/laavu ./cmd/laavud && CGO_ENABLED=0 go build -o bin/linkstub ./test/linkstub) || fail "build"
[[ -z "${NO_RESET:-}" ]] && "$(dirname "$0")/reset.sh"

scp -P "$VM_SSH_PORT" -i "$VM_SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
  "$ROOT/bin/laavu" "$ROOT/bin/laavud" "$ROOT/bin/linkstub" ubuntu@127.0.0.1:/tmp/ || fail "scp"
vm_ssh 'sudo /tmp/laavu install --host localhost --agent demo >/tmp/install.log 2>&1' || { vm_ssh 'tail -30 /tmp/install.log'; fail "install"; }
ok "installed with agent demo"

vm_ssh 'nohup /tmp/linkstub --listen 127.0.0.1:7800 --key /tmp/console.key >/tmp/stub.log 2>&1 &'
sleep 1
stub() { vm_ssh "curl -sf $*"; }
job() { vm_ssh "curl -s -X POST localhost:7800/job -d '$1'"; }
status_of() { sed -n 's/.*"status": "\([a-z]*\)".*/\1/p' | head -1; }

# Binding with a pending code: laavu link prints it, the "owner" confirms it in the console.
vm_ssh 'sudo nohup /usr/local/bin/laavu link --console ws://127.0.0.1:7800/v1 >/tmp/link.log 2>&1 &'
code=""
for _ in $(seq 30); do
  code=$(vm_ssh "sed -n 's/.*with this code: //p' /tmp/link.log" || true)
  [[ -n "$code" ]] && break
  sleep 1
done
[[ -n "$code" ]] || { vm_ssh 'cat /tmp/link.log /tmp/stub.log'; fail "no binding code"; }
stub "-X POST 'localhost:7800/confirm?code=$code'" || fail "confirm"
for _ in $(seq 30); do vm_ssh 'grep -q "^Linked" /tmp/link.log' && break; sleep 1; done
vm_ssh 'grep -q "^Linked" /tmp/link.log' || { vm_ssh 'cat /tmp/link.log'; fail "not linked"; }
ok "bound with code $code"

vm_ssh 'systemctl is-active laavu-link.service >/dev/null' || fail "laavu-link.service is not running"
[[ $(vm_ssh "ps -eo user:20,args | awk '\$2 ~ /laavu\$/ && \$3 == \"link\" && \$4 == \"run\" {print \$1}'") == "laavu-link" ]] \
  || fail "link process does not run as laavu-link"
[[ $(vm_ssh 'sudo stat -c "%U:%G %a" /etc/laavu/link.toml') == "root:laavu-link 640" ]] || fail "link.toml ownership"
vm_ssh 'sudo runuser -u laavu-link -- test -w /etc/laavu/link.toml' && fail "laavu-link can write link.toml"
vm_ssh 'sudo runuser -u laavu-link -- cat /etc/shadow >/dev/null 2>&1' && fail "laavu-link reads /etc/shadow"
vm_ssh "python3 -c 'import socket; socket.socket(socket.AF_UNIX).connect(\"/run/laavu/link-helper.sock\")' 2>&1 | grep -q PermissionError" \
  || fail "the owner user reaches the helper socket"
ok "privileges: link unprivileged, config root-only, helper socket closed to others"

for _ in $(seq 30); do stub localhost:7800/servers | grep -q '"schema"' && break; sleep 1; done
servers=$(stub localhost:7800/servers)
grep -q '"schema": 1' <<<"$servers" || { echo "$servers"; fail "no report"; }
grep -q '"agent-1"' <<<"$servers" || fail "agents are not numbered"
grep -q 'demo' <<<"$servers" && fail "report shows the agent name"
ok "report arrives without agent names"

[[ $(job '{"op":"firewall.allow","params":{"port":8099}}' | status_of) == done ]] || fail "firewall.allow"
vm_ssh 'sudo ufw status | grep -q 8099' || fail "port 8099 is not open"
[[ $(job '{"op":"agent.create","params":{"name":"second"}}' | status_of) == refused ]] || fail "agents right is off, job must be refused"
[[ $(job '{"op":"firewall.allow","params":{"port":"22 && id"}}' | status_of) == refused ]] || fail "bad params"
ok "jobs follow rights and schemas"

vm_ssh 'sudo laavu link rights agents=on >/dev/null'
sleep 6
[[ $(job '{"op":"agent.create","params":{"name":"second"}}' | status_of) == done ]] || fail "agent.create with the right on"
vm_ssh 'id lv-second >/dev/null' || fail "agent second was not created"
ok "rights change takes effect"

vm_ssh 'sudo laavu link status' | grep -q '^Connected' || fail "link status"
[[ $(job '{"op":"link.unlink"}' | status_of) == done ]] || fail "unlink from the console"
sleep 3
vm_ssh 'test -e /etc/laavu/link.toml' && fail "binding is still there"
vm_ssh 'systemctl is-active laavu-link.service >/dev/null' && fail "laavu-link still runs"
ok "console unlink removes the binding"

code=$(stub "-X POST localhost:7800/code")
vm_ssh "sudo laavu link --console ws://127.0.0.1:7800/v1 --code $code >/tmp/link2.log 2>&1" || { vm_ssh 'cat /tmp/link2.log'; fail "link with an issued code"; }
for _ in $(seq 20); do stub localhost:7800/servers | grep -q '"key"' && break; sleep 1; done
vm_ssh 'sudo laavu unlink' | grep -q 'console was told' || fail "laavu unlink did not reach the console"
vm_ssh 'grep -q unlinked /tmp/stub.log' || fail "stub did not see unlink"
ok "issued code and laavu unlink"
log "e2e-link: all checks passed"
