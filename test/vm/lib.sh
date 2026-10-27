# Shared part of the test VM scripts. Included via source.
set -euo pipefail

VM_NAME="${VM_NAME:-laavu-test}"
VM_CPUS="${VM_CPUS:-2}"
VM_RAM="${VM_RAM:-2048}"
VM_DISK_MB="${VM_DISK_MB:-20480}"
VM_SSH_PORT="${VM_SSH_PORT:-2222}"
VM_PASSWORD="${VM_PASSWORD:-laavu-test}"
VM_IMAGE="${VM_IMAGE:-$HOME/vm-images/noble-server-cloudimg-amd64.vmdk}"
VM_WORK="${VM_WORK:-$HOME/vm-images/$VM_NAME}"
VM_SSH_KEY="${VM_SSH_KEY:-$HOME/.ssh/id_ed25519}"
# Ports forwarded one-to-one: the app, the preview origin, dev servers.
VM_FORWARD_PORTS="${VM_FORWARD_PORTS:-8443 8444 8450 8451 8452 8453 8454 8455 8456 8457 8458 8459}"

SSH_OPTS=(-p "$VM_SSH_PORT" -i "$VM_SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5)

log() { printf '[vm] %s\n' "$*" >&2; }

vm_exists() { VBoxManage showvminfo "$VM_NAME" >/dev/null 2>&1; }

vm_running() { VBoxManage list runningvms | grep -q "^\"$VM_NAME\" "; }

vm_ssh() { ssh "${SSH_OPTS[@]}" ubuntu@127.0.0.1 "$@"; }

wait_ssh() {
  local deadline=$((SECONDS + ${1:-300}))
  until vm_ssh true 2>/dev/null; do
    if ((SECONDS > deadline)); then log "ssh did not come up"; return 1; fi
    sleep 3
  done
}

vm_start() {
  vm_running || VBoxManage startvm "$VM_NAME" --type headless >/dev/null
  wait_ssh
}

vm_stop() {
  vm_running || return 0
  vm_ssh 'sudo systemctl poweroff' 2>/dev/null || true
  local deadline=$((SECONDS + 120))
  while vm_running; do
    if ((SECONDS > deadline)); then VBoxManage controlvm "$VM_NAME" poweroff >/dev/null; break; fi
    sleep 2
  done
  # VirtualBox does not release the session lock right away.
  sleep 2
}
