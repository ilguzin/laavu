#!/usr/bin/env bash
# Creates the test VM from the Ubuntu 24.04 cloud image with a clean snapshot.
# Running it again does nothing if the VM already exists.
source "$(dirname "$0")/lib.sh"

if vm_exists; then
  log "VM $VM_NAME already exists"
  vm_start
  exit 0
fi

[[ -f "$VM_IMAGE" ]] || { log "image $VM_IMAGE not found"; exit 1; }
mkdir -p "$VM_WORK"

# seed ISO for cloud-init NoCloud
seed="$VM_WORK/seed"
rm -rf "$seed" && mkdir -p "$seed"
cat >"$seed/meta-data" <<META
instance-id: $VM_NAME
local-hostname: $VM_NAME
META
cat >"$seed/user-data" <<USER
#cloud-config
users:
  - name: ubuntu
    groups: [sudo]
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    lock_passwd: false
    ssh_authorized_keys:
      - $(cat "$VM_SSH_KEY.pub")
chpasswd:
  expire: false
  users:
    - name: ubuntu
      password: $VM_PASSWORD
      type: text
ssh_pwauth: true
package_update: false
USER
docker run --rm --network host -v "$VM_WORK:/w" alpine:3.20 sh -c \
  'apk add --no-cache xorriso >/dev/null && xorriso -as mkisofs -quiet -V cidata -J -R -o /w/seed.iso /w/seed/user-data /w/seed/meta-data && chown '"$(id -u):$(id -g)"' /w/seed.iso'

VBoxManage createvm --name "$VM_NAME" --ostype Ubuntu_64 --register >/dev/null
vmdir="$(dirname "$(VBoxManage showvminfo "$VM_NAME" --machinereadable | sed -n 's/^CfgFile="\(.*\)"$/\1/p')")"
disk="$vmdir/$VM_NAME.vdi"
VBoxManage clonemedium disk "$VM_IMAGE" "$disk" --format VDI >/dev/null
VBoxManage modifymedium disk "$disk" --resize "$VM_DISK_MB" >/dev/null

VBoxManage modifyvm "$VM_NAME" --cpus "$VM_CPUS" --memory "$VM_RAM" --nic1 nat \
  --uart1 0x3F8 4 --uartmode1 file "$VM_WORK/console.log" \
  --audio-enabled off --graphicscontroller vmsvga --boot1 disk
VBoxManage modifyvm "$VM_NAME" --natpf1 "ssh,tcp,127.0.0.1,$VM_SSH_PORT,,22"
for p in $VM_FORWARD_PORTS; do
  VBoxManage modifyvm "$VM_NAME" --natpf1 "p$p,tcp,127.0.0.1,$p,,$p"
done
VBoxManage storagectl "$VM_NAME" --name SATA --add sata --controller IntelAhci --portcount 2
VBoxManage storageattach "$VM_NAME" --storagectl SATA --port 0 --device 0 --type hdd --medium "$disk"
VBoxManage storageattach "$VM_NAME" --storagectl SATA --port 1 --device 0 --type dvddrive --medium "$VM_WORK/seed.iso"

log "boot and cloud-init"
vm_start
vm_ssh 'cloud-init status --wait >/dev/null; cloud-init status'

vm_stop
VBoxManage storageattach "$VM_NAME" --storagectl SATA --port 1 --device 0 --type dvddrive --medium emptydrive
VBoxManage snapshot "$VM_NAME" take clean --description "clean Ubuntu 24.04 after cloud-init" >/dev/null
vm_start
log "VM $VM_NAME is ready: ssh -p $VM_SSH_PORT ubuntu@127.0.0.1, snapshot clean"
