#!/usr/bin/env bash
# Шаг 0.6, часть 1 (root): пользователи lv-a, lv-b, procview, hidepid, слайсы, nftables. Потом перезагрузка.
set -eux
apt-get install -y -q nftables python3 curl >/dev/null
groupadd -f procview
groupadd -f laavu-agents
for u in lv-a lv-b; do
  id $u >/dev/null 2>&1 || useradd -m -d /srv/laavu/agents/${u#lv-} -s /bin/bash -G laavu-agents -c "umask=077" $u
  chmod 0700 /srv/laavu/agents/${u#lv-}
  passwd -l $u
  loginctl enable-linger $u
done
usermod -aG procview ubuntu
grep -q hidepid /etc/fstab || echo 'proc /proc proc defaults,hidepid=invisible,gid=procview 0 0' >> /etc/fstab
A=$(id -u lv-a); B=$(id -u lv-b)
mkdir -p /etc/systemd/system/user-$A.slice.d /etc/systemd/system/user-$B.slice.d
printf '[Slice]\nSocketBindAllow=tcp:20000-20999\nSocketBindDeny=any\nMemoryMax=1G\nTasksMax=512\n' > /etc/systemd/system/user-$A.slice.d/laavu.conf
printf '[Slice]\nSocketBindAllow=tcp:21000-21999\nSocketBindDeny=any\nMemoryMax=1G\nTasksMax=512\n' > /etc/systemd/system/user-$B.slice.d/laavu.conf
systemctl daemon-reload
cat > /etc/nftables-laavu.nft <<NFT
table inet laavu
delete table inet laavu
table inet laavu {
  chain output {
    type filter hook output priority 0; policy accept;
    oif "lo" tcp dport 20000-20999 meta skuid != $A meta skuid { $B } reject
    oif "lo" tcp dport 21000-21999 meta skuid != $B meta skuid { $A } reject
  }
}
NFT
nft -f /etc/nftables-laavu.nft
nft list table inet laavu
