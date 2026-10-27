#!/usr/bin/env bash
# Restores the test VM to the clean snapshot and starts it.
source "$(dirname "$0")/lib.sh"
vm_exists || { log "VM $VM_NAME does not exist, run create.sh first"; exit 1; }
if vm_running; then VBoxManage controlvm "$VM_NAME" poweroff >/dev/null; sleep 2; fi
VBoxManage snapshot "$VM_NAME" restore clean >/dev/null
vm_start
log "VM $VM_NAME restored to clean"
