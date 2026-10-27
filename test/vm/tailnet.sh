#!/usr/bin/env bash
# Expose Laavu on the test VM to other tailnet machines: forward VirtualBox ports
# to the host's tailnet address and switch the server to the host name (*.ts.net).
# Run again after test/vm/reset.sh: the snapshot also restores the VM settings.
#   test/vm/tailnet.sh            — name and address come from the host's tailscale status
source "$(dirname "$0")/lib.sh"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
read -r TS_NAME TS_IP < <(tailscale status --json | python3 -c 'import json,sys; s=json.load(sys.stdin)["Self"]; print(s["DNSName"].rstrip("."), s["TailscaleIPs"][0])')
[[ -n "$TS_NAME" && -n "$TS_IP" ]] || { log "host is not in a tailnet"; exit 1; }
for p in 8443 8444 $(seq 8450 8459); do
  VBoxManage controlvm "$VM_NAME" natpf1 delete "ts$p" 2>/dev/null || true
  VBoxManage controlvm "$VM_NAME" natpf1 "ts$p,tcp,$TS_IP,$p,,$p"
done
log "ports 8443, 8444, 8450-8459 forwarded to $TS_IP"
if vm_ssh 'test -x /usr/local/bin/laavu'; then
  vm_ssh "sudo laavu install --host $TS_NAME --skip-apt" | grep -v '^\['
else
  scp -P "$VM_SSH_PORT" -i "$VM_SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    "$ROOT/bin/laavu" "$ROOT/bin/laavud" ubuntu@127.0.0.1:/tmp/
  vm_ssh "cd /tmp && sudo ./laavu install --host $TS_NAME --agent demo --install-claude" | grep -v '^\['
fi
vm_ssh 'sudo cat /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt' >"$HOME/laavu-test-root.crt"
log "CA root for the browser: $HOME/laavu-test-root.crt"
log "address: https://$TS_NAME:8443 (new registration link: test/vm/ssh.sh sudo laavud reset-auth)"
