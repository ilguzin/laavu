# Checks on the test VM

Before writing any code we checked how tmux and the isolation measures behave on Ubuntu 24.04. VM: Ubuntu 24.04.5 LTS from the cloud image, 2 CPU, 2 GB, 20 GB disk, systemd 255.4-1ubuntu8.18, tmux 3.4. Checked 2026-10-24.

## VM

Scripts are in `test/vm/`, parameters come from environment variables (`VM_NAME`, `VM_CPUS`, `VM_RAM`, `VM_DISK_MB`, `VM_SSH_PORT`, `VM_PASSWORD`, `VM_FORWARD_PORTS`), and the shared code is in `lib.sh`.

- `./create.sh` makes a VDI from `~/vm-images/noble-server-cloudimg-amd64.vmdk`, grown to 20 GB, NoCloud seed ISO, forwarding of `127.0.0.1:2222→22` and of `8443`, `8444`, `8450–8459` one to one, cloud-init, snapshot `clean`, start. Run it again and it only starts the VM.
- `./reset.sh` rolls back to `clean`, starts the VM and waits for ssh. Takes about 30 s.
- `./ssh.sh [command]` connects to `ubuntu@127.0.0.1:2222` with the key `~/.ssh/id_ed25519`, skipping known_hosts.
- `./destroy.sh` deletes the VM and its disks.

`ubuntu` has passwordless sudo, the host key, and the password `laavu-test`; password login over ssh is enabled for testing `laavu install --ssh`. The VM console log is `~/vm-images/laavu-test/console.log`.

Pitfalls. Docker bridge networking doesn't work on the host (apk hangs), so the ISO is built with `--network host`. Once the seed disk is removed, cloud-init in the `clean` snapshot shows `disabled`. That's fine, we don't need it again.

The experiment scripts sit next to them and can be rerun: `test/vm/stand/tmux-hooks.sh` (as a user), `stand/iso-setup.sh` and `stand/iso-check.sh` (as root, with a reboot between them): `./ssh.sh 'sudo bash -s' < stand/iso-setup.sh`.

## tmux 3.4

Confirmed:

- A config through `tmux -L laavu -f conf` is accepted without errors: `status off`, `prefix None`, `default-terminal tmux-256color`, `set -ga terminal-features ",xterm-256color:RGB:usstyle:hyperlinks:sync:clipboard"`, `set-clipboard on`, `escape-time 10`, `extended-keys on` (a server option, `set -s`), `history-limit`, `aggressive-resize`, `window-size latest`, `remain-on-exit on`.
- The hooks `session-created`, `session-closed`, `session-renamed`, `pane-died` fire also for sessions created by a separate `tmux -L laavu new -d -s x` call outside the worker. `session-closed` also fires when the last session closes, before the server exits.
- User options `@laavu-*` move with the session on `rename-session`.
- `list-sessions -F` returns the server's `#{pid}` and `#{start_time}`, `#{session_path}` (the folder from `-c`), `#{pane_dead}`, `#{pane_dead_status}`, `#{pane_start_command}`.

Which format to use in which hook (the others give a different or empty session):

| Hook | Session name | Notes |
|---|---|---|
| `session-created` | `#{hook_session_name}` (`#{session_name}` is also correct) | |
| `session-renamed` | `#{hook_session_name}` (the new name) | the old name is not passed, a snapshot reconciliation is needed |
| `session-closed` | only `#{hook_session_name}` | `#{session_name}` here is another, live session |
| `pane-died` | `#{session_name}`, the pane is `#{hook_pane}` | `#{hook_session_name}` is empty |

Gotchas:

- **Inside `run-shell` formats must be escaped.** `#{hook_session}` expands to `$0`, and the shell turns it into `sh`. This works: `set-hook -g session-closed "run-shell -b 'laavud hook tmux session-closed #{q:hook_session_name}'"`, with `#{q:…}` inside single quotes.
- `pane_start_command` is empty for a session with the default shell, and for a session with a command it is a string in tmux quotes (`"sleep 1; exit 3"`). So the snapshot keeps the start command in our own option (`@laavu-cmd`) and falls back to `pane_start_command` only for sessions created outside the worker.
- `show -g` takes one option per call.

## Isolation

Users `lv-a` (uid 1001) and `lv-b` (1002): `useradd -m -d /srv/laavu/agents/<name> -s /bin/bash -G laavu-agents -c "umask=077"`, home `0700`, `passwd -l`, `loginctl enable-linger`.

| Measure | Result |
|---|---|
| `hidepid=invisible,gid=procview` in fstab (`proc /proc proc defaults,hidepid=invisible,gid=procview 0 0`) | works after a reboot. logind, `loginctl`, linger, `user@.service`, `systemctl --user`, `systemd-run --user --scope` work for the agent **without** a `SupplementaryGroups=procview` drop-in for logind and polkit, with no errors in the journal. `lv-a` does not see `lv-b`'s processes, `ubuntu` in `procview` does |
| Home `0700` | `lv-a` cannot read `lv-b`'s home |
| umask | GECOS `umask=077` via pam_umask applies both in `sudo -u` and in `user@.service` (umask `0077`): `lv-b`'s files in `/tmp` are `-rw-------`. The worker's system unit (no PAM) still needs `UMask=0077` in the unit |
| `MemoryMax`, `TasksMax` in `user-<uid>.slice.d/laavu.conf` | applied (`memory.max`, `pids.max` of the slice) |
| **`SocketBindAllow` / `SocketBindDeny`** | **do not work at all.** systemd in Ubuntu 24.04 is built with `-BPF_FRAMEWORK`, so the setting is silently ignored both on the slice and even on a service (`systemd-run -p SocketBindDeny=any` binds the port). `systemctl show` shows the setting, but there is no BPF program on the cgroup |
| `IPAddressDeny` | works (built-in BPF program, no BPF_FRAMEWORK needed), if we ever want it |
| nftables by `meta skuid` | works: `lv-a` cannot connect to `lv-b`'s server in its range via `127.0.0.1`, `::1`, or the VM's external address (also through `lo`). `lv-b` itself, root and `ubuntu` can connect. Outgoing traffic (curl, DNS) is not affected, `bind()` on port 0 works |
| `sudo -n true` as the agent | refused |

Working nftables rule (file `/etc/laavu/nftables.nft`, reloads without errors):

```
table inet laavu
delete table inet laavu
table inet laavu {
  chain output {
    type filter hook output priority 0; policy accept;
    oif "lo" tcp dport 20000-20999 meta skuid != 1001 meta skuid { 1002 } reject
    oif "lo" tcp dport 21000-21999 meta skuid != 1002 meta skuid { 1001 } reject
  }
}
```

The table doesn't survive a reboot on its own. The packaged `nftables.service` won't do, because its `/etc/nftables.conf` starts with `flush ruleset` and would wipe the UFW rules. A unit of our own works. We checked it with UFW enabled, across a reboot:

```
# /etc/systemd/system/laavu-nftables.service
[Unit]
Description=Laavu: nftables rules between agents
Wants=network-pre.target
Before=network-pre.target
After=ufw.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/laavu/nftables.nft
ExecStop=/usr/sbin/nft delete table inet laavu
[Install]
WantedBy=multi-user.target
```

The owner of a listening socket is visible in `/proc/net/tcp` and `/proc/net/tcp6` (8th column, uid) to any user. `hidepid` doesn't change that.

## What to change in the architecture

1. **`SocketBindAllow` is unusable on Ubuntu 24.04.** systemd can't restrict which ports an agent listens on. What we do instead:
   - The nftables rule above protects an agent's range from other agents' connections. It works, and on its own it stops agents from connecting to each other's services inside those ranges.
   - An agent can grab a port in a neighbor's range before the neighbor does. The `laavud` proxy (flow 6) would then show the user the wrong agent's server. So before connecting, the proxy compares the uid of the listening socket's owner from `/proc/net/tcp{,6}` with the agent's uid and refuses if they differ.
   - Ports outside all ranges (ephemeral Playwright or Jupyter listeners, for example) are reachable by other agents. A rule like "an agent may reach `lo` only in its own range" would close them, but it would also break tools that listen on a random port. We accept this as residual risk, along with abstract unix sockets.
   - In `check-isolation` the probe "take a port outside your range" is dropped or turned into a warning. It checks instead that `lv-a` can't connect to a `lv-b` listener in `lv-b`'s range.
   - In the isolation measures table (now in [`arch-laavu.md`](arch-laavu.md)), replace the `SocketBindAllow` row with nftables + the owner check in the proxy. If we ever really need to ban `bind`, the options are a custom `cgroup/bind4|bind6` BPF program or systemd with BPF_FRAMEWORK (check in Ubuntu 26.04).
2. nftables gets its own unit, `laavu-nftables.service`. Leave `nftables.service` and `/etc/nftables.conf` alone.
3. logind and polkit need no drop-in under `hidepid`. If the installer was going to add one, drop it.
4. tmux hooks use the formats from the table above, always with `#{q:…}` in single quotes. `session-renamed` does not give the old name, `pane-died` gives the name via `#{session_name}`, and `session-closed` only via `#{hook_session_name}`. Reconciling the snapshot with the session list on every hook works, as planned.
5. umask: pam_umask with `umask=077` in GECOS works for everything that goes through PAM, including the systemd user instance. `UMask=0077` is needed only in the worker's system unit.
