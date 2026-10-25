#!/usr/bin/env bash
# Удаление тестовой ВМ вместе с дисками и снапшотами.
source "$(dirname "$0")/lib.sh"
vm_exists || exit 0
if vm_running; then VBoxManage controlvm "$VM_NAME" poweroff >/dev/null; sleep 2; fi
VBoxManage unregistervm "$VM_NAME" --delete
rm -rf "$VM_WORK"
log "ВМ $VM_NAME удалена"
