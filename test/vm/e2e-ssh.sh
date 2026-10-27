#!/usr/bin/env bash
# Install from a laptop and updates:
# fresh VM with a password → laavu install --ssh with a signed release → key-only
# login → laavu update to a new version → a broken release is rolled back.
source "$(dirname "$0")/lib.sh"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
export PATH="$HOME/.local/opt/go/bin:$PATH"
fail() { log "FAIL: $*"; exit 1; }
ok() { log "ok: $*"; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export XDG_CONFIG_HOME="$WORK/config"

# A project key for the test and releases 0.0.1, 0.0.2 and a broken 0.0.3.
"$ROOT/bin/laavu" release keygen --out "$WORK/release.key" >"$WORK/keygen.txt" 2>/dev/null || (cd "$ROOT" && go build -o bin/ ./cmd/laavu && "$ROOT/bin/laavu" release keygen --out "$WORK/release.key" >"$WORK/keygen.txt")
PUB=$(tail -1 "$WORK/keygen.txt")
build_release() { # version dir [broken]
  local v=$1 dir=$2
  mkdir -p "$dir"
  local ld="-X main.Version=$v -X github.com/ilguzin/laavu/internal/release.ProjectKey=$PUB"
  (cd "$ROOT" && CGO_ENABLED=0 go build -ldflags "$ld" -o "$dir/laavu-linux-amd64" ./cmd/laavu) || fail "build laavu $v"
  if [[ -n "${3:-}" ]]; then
    printf '#!/bin/sh\n[ "$1" = version ] && echo %s && exit 0\nexit 1\n' "$v" >"$dir/laavud-linux-amd64"
    chmod 755 "$dir/laavud-linux-amd64"
  else
    (cd "$ROOT" && CGO_ENABLED=0 go build -ldflags "$ld" -o "$dir/laavud-linux-amd64" ./cmd/laavud) || fail "build laavud $v"
  fi
  "$ROOT/bin/laavu" release manifest --dir "$dir" --version "$v"
  "$ROOT/bin/laavu" release sign --dir "$dir" --key "$WORK/release.key"
}
build_release 0.0.1 "$WORK/r1"
build_release 0.0.2 "$WORK/r2"
build_release 0.0.3 "$WORK/r3" broken
# laavu on the "laptop" comes from release 0.0.1, with the project key built in.
cp "$WORK/r1/laavu-linux-amd64" "$WORK/laavu"
ssh-keygen -q -t ed25519 -N '' -C owner-test -f "$WORK/owner"

[[ -z "${NO_RESET:-}" ]] && "$(dirname "$0")/reset.sh"

log "laavu install --ssh with a password"
out=$(printf '%s\n' "$VM_PASSWORD" | "$WORK/laavu" install --ssh "ubuntu@127.0.0.1:$VM_SSH_PORT" --identity none --yes \
  --release "$WORK/r1" --owner-pubkey "$WORK/owner.pub" --host localhost --agent demo 2>&1) || { echo "$out"; fail "install --ssh"; }
echo "$out" | grep -q "Server key fingerprint: SHA256:" || fail "no host fingerprint"
echo "$out" | grep -q "Release 0.0.1: signature verified" || fail "no signature check on the laptop"
echo "$out" | grep -q "signature and files match" || fail "no signature check on the server"
link=$(echo "$out" | grep -o 'https://localhost:8443/register#[A-Za-z0-9_-]*' | head -1)
[[ -n "$link" ]] || { echo "$out"; fail "no registration link"; }
echo "$link" >"$ROOT/test/vm/.last-register-link"
echo "$out" | grep -q "key only" || { echo "$out"; fail "password login not disabled"; }
ok "install from the laptop, link: $link"

ssh -p "$VM_SSH_PORT" -i "$WORK/owner" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR ubuntu@127.0.0.1 true || fail "login with the owner key"
if sshpass -V >/dev/null 2>&1; then
  sshpass -p "$VM_PASSWORD" ssh -p "$VM_SSH_PORT" -o PubkeyAuthentication=no -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@127.0.0.1 true 2>/dev/null && fail "password login still works"
fi
vm_ssh 'sudo sshd -T | grep -qx "passwordauthentication no"' || fail "PasswordAuthentication is not disabled"
vm_ssh "sudo cat /etc/laavu/release.pub" | grep -qx "$PUB" || fail "project key is not pinned"
[[ "$(vm_ssh 'laavud version')" == 0.0.1 ]] || fail "version after install"
ok "key-only login, project key pinned"

# Reconnecting with the same host key needs no confirmation; a spoofed one is refused.
sed -i 's/SHA256:[A-Za-z0-9+\/]*/SHA256:AAAAspoofed/' "$XDG_CONFIG_HOME/laavu/known_hosts"
out=$(printf '\n' | "$WORK/laavu" install --ssh "ubuntu@127.0.0.1:$VM_SSH_PORT" --identity "$WORK/owner" --release "$WORK/r1" 2>&1) && fail "spoofed host key accepted"
echo "$out" | grep -q "changed" || { echo "$out"; fail "no host key change message"; }
ok "host key change is detected"

log "laavu update"
scp -P "$VM_SSH_PORT" -i "$VM_SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -r "$WORK/r2" "$WORK/r3" ubuntu@127.0.0.1:/tmp/ || fail "copy releases"
vm_ssh 'sudo laavu update laavud --from /tmp/r2' || fail "update 0.0.2"
[[ "$(vm_ssh 'laavud version')" == 0.0.2 ]] || fail "version after update"
ok "update to 0.0.2"
vm_ssh 'sudo laavu update laavud --from /tmp/r3 --timeout 8s' && fail "broken release was installed"
[[ "$(vm_ssh 'laavud version')" == 0.0.2 ]] || fail "rollback did not restore 0.0.2"
vm_ssh 'systemctl is-active laavud' | grep -qx active || fail "laavud after rollback"
ok "broken release rolled back to 0.0.2"
# Signed with someone else's key.
"$ROOT/bin/laavu" release keygen --out "$WORK/evil.key" >/dev/null
"$ROOT/bin/laavu" release sign --dir "$WORK/r2" --key "$WORK/evil.key"
scp -P "$VM_SSH_PORT" -i "$VM_SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "$WORK/r2/manifest.json.sig" ubuntu@127.0.0.1:/tmp/r2/ || fail "copy"
out=$(vm_ssh 'sudo laavu update laavud --from /tmp/r2 --force' 2>&1) && fail "foreign signature accepted"
echo "$out" | grep -q "release signature does not match" || fail "no signature message: $out"
ok "release with a foreign signature rejected"
log "ALL PASSED"
