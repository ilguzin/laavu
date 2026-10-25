#!/usr/bin/env bash
# Откат тестовой ВМ к снапшоту clean и старт.
source "$(dirname "$0")/lib.sh"
vm_exists || { log "ВМ $VM_NAME нет, сначала create.sh"; exit 1; }
if vm_running; then VBoxManage controlvm "$VM_NAME" poweroff >/dev/null; sleep 2; fi
VBoxManage snapshot "$VM_NAME" restore clean >/dev/null
vm_start
log "ВМ $VM_NAME откачена к clean"
