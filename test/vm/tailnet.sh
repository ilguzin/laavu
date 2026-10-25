#!/usr/bin/env bash
# Открыть Laavu на тестовой ВМ для других машин tailnet: проброс портов VirtualBox
# на адрес хоста в tailnet и перевод сервера на имя хоста (*.ts.net).
# После test/vm/reset.sh запускать заново: снапшот откатывает и настройки ВМ.
#   test/vm/tailnet.sh            — имя и адрес берутся из tailscale status хоста
source "$(dirname "$0")/lib.sh"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
read -r TS_NAME TS_IP < <(tailscale status --json | python3 -c 'import json,sys; s=json.load(sys.stdin)["Self"]; print(s["DNSName"].rstrip("."), s["TailscaleIPs"][0])')
[[ -n "$TS_NAME" && -n "$TS_IP" ]] || { log "хост не в tailnet"; exit 1; }
for p in 8443 8444 $(seq 8450 8459); do
  VBoxManage controlvm "$VM_NAME" natpf1 delete "ts$p" 2>/dev/null || true
  VBoxManage controlvm "$VM_NAME" natpf1 "ts$p,tcp,$TS_IP,$p,,$p"
done
log "порты 8443, 8444, 8450–8459 проброшены на $TS_IP"
if vm_ssh 'test -x /usr/local/bin/laavu'; then
  vm_ssh "sudo laavu install --host $TS_NAME --skip-apt" | grep -v '^\['
else
  scp -P "$VM_SSH_PORT" -i "$VM_SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    "$ROOT/bin/laavu" "$ROOT/bin/laavud" ubuntu@127.0.0.1:/tmp/
  vm_ssh "cd /tmp && sudo ./laavu install --host $TS_NAME --agent demo --install-claude" | grep -v '^\['
fi
vm_ssh 'sudo cat /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt' >"$HOME/laavu-test-root.crt"
log "корень CA для браузера: $HOME/laavu-test-root.crt"
log "адрес: https://$TS_NAME:8443 (новая ссылка регистрации: test/vm/ssh.sh sudo laavud reset-auth)"
