# Installation, isolation and releases

How `laavu install`, the `laavu` utility, the privileged `laavud priv` operations, agent isolation and releases work in the code. The daemon is described in [`arch-daemon.md`](arch-daemon.md), the worker in [`arch-worker.md`](arch-worker.md), the client in [`arch-client.md`](arch-client.md). Building and the test VM are in [`dev.md`](dev.md).

## Two binaries

`laavu` is the tool for the server owner. `laavud` contains the daemon, the worker, the hooks and the privileged operations. Both come from one module (`Makefile`, `CGO_ENABLED=0`, version set with `-X main.Version`). Privileged code lives in `internal/priv` and both binaries call it. `laavu agent …` and `laavud priv …` end up in the same `priv.Main`.

## `laavu` commands

Argument parsing is in `cmd/laavu/main.go`. Almost every command needs root and checks for it itself (`os.Geteuid() != 0`).

| Command | What it does | Code |
|---|---|---|
| `install --domain D [--email E] \| --sslip [--public-ip IP] \| --tailscale [--tailscale-authkey KEY] [--tailscale-hostname NAME] \| --host NAME`, then `[--agent NAME] [--install-claude] [--owner-key …] [--owner-user U] [--harden-ssh] [--skip-apt] [--release-key …] [--no-link]` | installs Laavu on this server, see below | `install.Run`, `internal/install/install.go` |
| `install --ssh USER@IP[:PORT] [--release …] [--identity …] [--owner-pubkey …] [--yes]` | installs from a laptop onto a remote server | `install.RemoteInstall`, `internal/install/remote.go` |
| `agent create NAME [--memory 2G] [--tasks N] [--cpu-weight N] [--install-claude]` | a new agent with all isolation measures | `priv.CreateAgent` |
| `agent delete NAME [--keep-home]` | deletes an agent | `priv.DeleteAgent` |
| `agent list` | agents: name, user, uid, ports, home | `priv.listAgents` |
| `agent limits NAME [--memory] [--tasks] [--cpu-weight]` | without flags shows the slice limits, with flags changes them | `priv.limitsCmd` |
| `check-isolation [A B] [--json]` | isolation probes for one pair or all pairs | `priv.checkIsolationCmd`, `internal/priv/isolation.go` |
| `check [--json]` | server status report; exit code 1 on problems | `install.Check`, `internal/install/maint.go` |
| `firewall status\|allow PORT\|deny PORT` | a wrapper over `ufw` that checks the port is a number | `install.Firewall` |
| `tailscale enable [--mode extra\|only] [--authkey KEY] [--hostname NAME]\|disable\|status` | Tailscale as an option | `internal/install/tailscale.go` |
| `update laavud\|templates\|os\|claude` | updates | `internal/install/update.go` |
| `restart laavud` | `systemctl restart laavud.service`, does not touch workers | `cmd/laavu/main.go`, `restart` |
| `cleanup` | logs older than 30 days, apt cache, agents' `~/tmp` older than a week | `install.Cleanup` |
| `diag` | service status, logs, nftables table, UFW, `/run/laavu`, `/proc`, config, tail of `priv.log` | `install.Diag` |
| `harden-ssh --owner-key … [--owner-user …]` | only the ssh step with passwords disabled | `install.HardenSSH` |
| `release keygen\|manifest\|sign\|verify` | project key and release signing | `cmd/laavu/main.go`, `internal/release` |
| `link [--console URL] [--code C]`, `link status\|rights`, `unlink` | link to a management console, see [`link.md`](link.md) | `internal/linkcmd` |
| `link run`, `link bind`, `link helper` | the link process, its binding child and the root helper; started by systemd units and `laavu link` | `internal/linkcmd`, on top of `pkg/link` |
| `version` | build version | — |

`laavud` subcommands that matter here (`cmd/laavud/main.go`):

| Command | What it does |
|---|---|
| `laavud priv create-agent\|delete-agent\|list\|limits\|check-isolation` | the same as `laavu agent …`; requires root |
| `laavud priv nft-sync` | rewrite and load the nftables rules; not shown in the help |
| `laavud priv probe JSON` | an isolation probe as the agent; does not need root, called from `CheckIsolation` |
| `laavud reset-auth` | erase all passkeys and sign-ins, print an administrator registration link |
| `laavud invite [--admin] [--name] [--agent] [-q] [--if-no-admin]` | a single-use registration link; `--if-no-admin` does nothing if an administrator already exists. A member gets access to the `--agent` agents (repeatable or comma-separated) once registered. Without `--agent` a single agent is granted by default; with several, the command asks on a terminal and grants nothing without one |
| `laavud admin add NAME` | grant the administrator role |
| `laavud access grant\|revoke USER AGENT...`, `laavud access list [USER]` | a member's access to agents; applies without a restart, the client picks up new agents on reload |
| `laavud access drop AGENT` | remove everyone's access to the agent, including access promised by unused links; called by agent deletion |

When `reset-auth`, `invite`, `admin` and `access` are run as root, they re-exec themselves as the `laavud` user through `runuser` (`asLaavud`). The database belongs to `laavud`, and SQLite journal files owned by root would break the daemon.

## `laavu install`

`install.Run` checks for root and the OS (`/etc/os-release` must say Ubuntu 24 or later), then runs the steps. Each one prints a line like `[N/11] …`. The owner user comes from `--owner-user` or `SUDO_USER`.

| # | Step | What it does | Function |
|---|---|---|---|
| 1 | packages | installs the missing ones from `tmux git caddy nftables ufw curl ca-certificates` (`dpkg-query`, then `apt-get install --no-install-recommends`); `--skip-apt` skips it | `stepPackages` |
| 2 | binaries | copies `laavu` and `laavud` from the directory of the running `laavu` to `/usr/local/bin` | `stepBinaries` |
| 3 | users and groups | system groups `laavu-agents` and `procview`, system user `laavud` (home `/var/lib/laavu`, `nologin`); the owner is added to `procview`; creates `/srv/laavu/agents` and `/var/log/laavu` | `stepUsers` |
| 4 | hidepid | the line `proc /proc proc defaults,hidepid=invisible,gid=<procview>` in `/etc/fstab` if there is no `hidepid=` yet, and `mount -o remount` right away | `stepHidepid` |
| 5 | ssh | the owner's key in `authorized_keys` (if given), `/etc/ssh/sshd_config.d/01-laavu.conf` with `DenyGroups laavu-agents`; with `--harden-ssh` also disables passwords and sets `PermitRootLogin prohibit-password` | `stepSSH` |
| 6 | configuration | `/etc/laavu/{agents,agent-template}`, the pinned release key, `laavud.toml`, `reserved`, `tmux.conf`, `/etc/tmpfiles.d/laavu.conf`, agent rule templates; `systemd-tmpfiles --create` | `stepConfig` |
| 7 | nftables | the `laavu-nftables.service` unit, rules for the current agents, `enable --now` | `stepNftables` |
| 8 | firewall | UFW: ssh and the mode's ports, `ufw --force enable` | `stepFirewall` |
| 9 | Caddy | `/etc/caddy/Caddyfile` from the template, `caddy validate`, restart on change | `stepCaddy` |
| 10 | laavud | units `laavud.service`, `laavud-worker@.socket`, `laavud-worker@.service`; restart of the daemon, `try-restart` of the workers; waits up to 10 seconds for `/run/laavu/http/app.sock`, otherwise prints the log | `stepDaemon` |
| 11 | first agent | `priv.CreateAgent` if `--agent` is set and user `lv-<name>` does not exist yet | `Run` |

With `--tailscale`, after step 11 the server moves into the tailnet (`tsSwitch`, mode `only`, after checking `sshd -T` again). At the end `printLink` runs `laavud invite --admin -q --if-no-admin` as `laavud` and prints the registration link for the first passkey. If an administrator already exists, it tells you to run `laavud reset-auth` for a new link. `--no-link` skips it: `install --ssh` with `--tailscale` prints the link only after the switch.

**ssh.** The file is called `01-laavu.conf` on purpose. In `sshd_config.d` the first value wins, and hosting providers turn passwords on in `50-cloud-init.conf`. The old `50-laavu.conf` is removed. After writing the file the step runs `sshd -t` (and deletes the file if that fails), then `systemctl reload ssh`. Passwords are never disabled unless the owner's key is in `authorized_keys`. Without it the step fails, so you can't lock yourself out of the server. Once applied, `sshd -T` confirms that `passwordauthentication no` and `denygroups laavu-agents` are really in effect.

**Address modes.** `--domain`, `--sslip`, `--tailscale` and `--host` decide what goes into `laavud.toml`, Caddy and UFW (`resolveAddress` in `address.go`, then `daemonConfig`, `caddyData`, `stepFirewall`). The first install needs exactly one of them and fails with the list otherwise: passkeys need a host name with HTTPS. A repeated install without flags takes the address from `laavud.toml`, and on a server already in the tailnet keeps its ts.net Caddyfile and firewall rules.

- `--sslip` is the domain column for the name `A-B-C-D.sslip.io`, which resolves to the server's IPv4 together with `view.A-B-C-D.sslip.io`. The address is `--public-ip` or the source address of the route to `1.1.1.1` (no packet is sent). A private or CGNAT address fails with a hint: behind NAT the server can't see its public IP. `install --ssh` to a public IPv4 passes it as `--public-ip` itself. sslip.io isn't in the Public Suffix List, so its Let's Encrypt limit is shared by everyone who uses it.
- `--tailscale` installs in the VM column on `localhost`, then runs `tailscale enable --mode only` with `--tailscale-authkey` and `--tailscale-hostname` (see Tailscale below), and only then prints the link, already on the ts.net address. While ssh still takes passwords (`sshd -T`, unless this run has `--owner-key` and `--harden-ssh`), it stops before installing. With `--ssh` the server installs with `--host localhost --no-link`, the laptop turns passwords off and moves the server into the tailnet over the connection that is still open, since public ssh closes with the switch; if passwords couldn't be turned off, it stops before the switch and prints what to run.

| | With a domain (`--domain`, `--sslip`) | Test VM (`--host NAME`, `--tailscale` before the switch) |
|---|---|---|
| App | `https://DOMAIN` | `https://NAME:8443` |
| View origin | `https://view.DOMAIN` | `https://NAME:8444` |
| Dev servers | ports `8450–8459` of the domain (`DevMode = "ports"`) | ports `8450–8459` |
| Certificate | Let's Encrypt, email from `--email` (optional) | Caddy internal CA, `tls internal { on_demand }` |
| UFW | ssh, 80, 443, `8450:8459` | ssh, `8443:8444`, `8450:8459` |

The Caddyfile has two sites. The app is proxied to `unix//run/laavu/http/app.sock`, and the view origin and dev server ports go to `aux.sock`. Caddy runs with `admin off`, because its API on `localhost:2019` would let an agent reconfigure the proxy. That means config changes only take effect by restarting Caddy.

A repeated install with an address flag (`--host`, `--domain`, `--sslip`) that gives a different address rewrites the addresses and the dev server mode in the existing `laavud.toml` and warns that passkeys have to be registered again, since they're bound to the name. A server already moved to its ts.net name keeps its address.

**VAPID.** There's no installer step for it. The daemon generates the keys on first start and keeps them in the database (`push.New` in `internal/push/push.go`).

**logind.** The installer doesn't add a logind drop-in. On the test bench `hidepid` works fine alongside logind, linger and `user@.service` without one ([`stand-vm.md`](stand-vm.md)).

**Idempotency.** Files are written with `writeIfChanged`: compare the content, write to a temp file, `rename`. Groups and users are created only if they're missing. `laavud.toml`, `reserved` and the templates in `agent-template/` are written only when they don't exist yet, because the owner edits them; of `laavud.toml` only the address fields change later, on an explicit address change. `release.pub` isn't overwritten either. Caddy restarts only when the Caddyfile changed. `laavud` always restarts. `test/vm/e2e-server.sh` covers reinstalling: a second `install` succeeds and prints the link again as long as no administrator has registered.

## Installing from a laptop: `install --ssh`

`install.RemoteInstall` runs on the owner's laptop. CI builds `laavu` for Linux, macOS (amd64, arm64) and Windows.

1. **Signature check before connecting.** With `--release DIR|https-URL` the manifest is verified with the project key (`release.PinnedKey`: `/etc/laavu/release.pub` if it exists, otherwise the embedded `ProjectKey`). Without `--release` it installs the local build sitting next to `laavu`, prints "Warning: installing a local build without a release signature.", and only does this if the build is for the same linux architecture.
2. **ssh.** It uses the built-in client from `golang.org/x/crypto/ssh`, so the system `ssh` isn't needed. Login works by key (`--identity`, default `~/.ssh/id_ed25519`, and `none` means password only), by password, or by keyboard-interactive. The password is asked for once without echo (`term.ReadPassword`) and only ever lives in process memory. If stdin isn't a terminal, it reads a line instead, which the automated tests rely on.
3. **Host fingerprint** (`hostKeyCallback`). On first connect it prints the SHA256 fingerprint and asks `[y/N]` (`--yes` skips the question for tests). Then it appends a `host fingerprint` line to `known_hosts` in the user config directory (`os.UserConfigDir()/laavu/known_hosts`). If the key changes later, the connection is refused with a hint about which line to delete.
4. **Permissions.** A non-root user needs passwordless `sudo -n`, otherwise it's an error. The architecture comes from `uname -m` (`x86_64` → amd64, `aarch64` → arm64).
5. **Copying.** The `laavu` and `laavud` binaries from the release (hashes checked against the manifest in `Verified.Fetch`) are piped through `cat >` into a `mktemp -d /tmp/laavu-install.XXXXXX` directory. That directory is removed at the end.
6. **Second signature check**, on the server, by its own `laavu release verify` with the key and `--file` for both binaries.
7. **Installation.** `sudo -n …/laavu install` with the install flags, `--owner-key` and `--release-key` (which pins the project key in `/etc/laavu/release.pub`). Output streams to the laptop terminal.
8. **Owner key.** Taken from `--owner-pubkey`, then `~/.ssh/id_ed25519.pub`. If neither exists, a new ed25519 key `~/.ssh/laavu_<host>` (0600) is generated.
9. **Disabling passwords.** Key login is tested over a fresh connection, and only after that does it call `laavu harden-ssh`. If the private key has a passphrase or key login fails, password login stays on and the tool tells you. Finally it prints `ssh -i KEY user@host`. The registration link was already in the install output.

## Privileges: who calls `priv`

The daemon never calls `priv`. The `laavud.service` unit runs with `NoNewPrivileges=yes` and an empty `CapabilityBoundingSet`, so it can't sudo, and the code has no sudoers or polkit rules. Only root creates and deletes agents, through `laavu agent …` or `laavud priv …`. There's no path from the client yet. Every operation is logged to `/var/log/laavu/priv.log` by `priv.Log` (time, `SUDO_USER` or `root`, operation).

```mermaid
flowchart TD
  owner["owner: sudo laavu agent create demo"] --> main["priv.Main"]
  main --> ca["priv.CreateAgent"]
  ca --> ua["useradd lv-demo, passwd --lock"]
  ca --> tpl["rule template in ~/.claude"]
  ca --> sl["user-UID.slice.d/laavu.conf"]
  ca --> toml["/etc/laavu/agents/demo.toml"]
  ca --> nft["SyncNftables: nft -f"]
  ca --> lg["loginctl enable-linger"]
  ca --> wk["systemctl enable --now laavud-worker@lv-demo"]
  ca --> rs["systemctl try-restart laavud"]
  ca --> chk["CheckIsolation with every agent"]
  chk --> pr["systemd-run --uid A in slice A: laavud priv probe"]
```

## Creating an agent: `CreateAgent`

The code is in `internal/priv/priv.go`. `names.Agent` validates the name. Creating an agent that already exists is an error if either the user or the agent is there. Only `install` is idempotent here, since it skips an existing agent.

| Measure | How it is done |
|---|---|
| User | `lv-<name>`, shell `/bin/bash`, supplementary group `laavu-agents` (used for `DenyGroups` in sshd and for access to the hook socket), password locked with `passwd --lock`. No other groups are given |
| Home | `/srv/laavu/agents/<name>`, `chmod 0700`, inside `tmp`, `.claude`, `.local/state/laavu`, `.ssh` with 0700; everything is handed to the agent by `chownR` (through `Lchown`, does not follow symlinks) |
| umask | `umask=077` in the GECOS field (`--comment`) for `pam_umask` and `UMASK=077` (`useradd --key`); units and `systemd-run` additionally set `UMask=0077` |
| Ports | the first free block of 1000 ports from `20000–59999` (`nextPorts`) |
| Resources | drop-in `/etc/systemd/system/user-<uid>.slice.d/laavu.conf`: `MemoryMax` (default 3/4 of server memory, `defaultMemory`), `TasksMax=1024`, `CPUWeight=100` |
| Network | an nftables rule by uid, see below. `SocketBindAllow` is not written: it does not work on Ubuntu 24.04 |
| tmux outside the worker cgroup | `loginctl enable-linger`, waiting up to 10 seconds for `/run/user/<uid>/bus` |
| Worker | `systemctl enable --now laavud-worker@lv-<name>.socket` and `.service` |
| Agent rules | `~/.claude/CLAUDE.md` and `~/.claude/settings.json` (0600) from the template; if the owner edited the template in `/etc/laavu/agent-template/`, that one is used, otherwise the embedded one. Existing files are not overwritten |
| Claude Code | with `--install-claude`, the official installer as the agent (`runuser -l … curl … \| bash`); an installation error does not fail the creation |

After that it runs `try-restart laavud`, because the daemon reads the agent list only at startup, and then `CheckIsolation` between the new agent and every existing one in both directions. The result is printed but doesn't undo the creation.

The template `deploy/agent/CLAUDE.md.tmpl` is filled with the name, user, ports and the path to `laavud`. It tells the agent where things go: work in top-level folders of the home, which names are reserved, where uploads land (`.laavu-uploads`), git with one deploy key per repository, ports from `LAAVU_PORTS` on `127.0.0.1`, long-running processes in `tmux -L laavu` sessions and temp files in `~/tmp`. `settings.json.tmpl` hooks `laavud hook claude` and `laavud hook guard` into Claude Code events and `laavud hook statusline` into the status line. What those do is in [`arch-daemon.md`](arch-daemon.md).

**nftables** (`internal/priv/nft.go`). `NftRules` builds the `inet laavu` table with an `output` chain. For each agent there's `oif "lo" tcp dport <its range> meta skuid { uids of the other agents } reject with tcp reset`. The agent itself, `laavud` and root aren't in the set, so they can connect. The table is replaced atomically (`table … delete table … table {…}` in one file). `SyncNftables` writes `/etc/laavu/nftables.nft`, loads it with `nft -f` and enables `laavu-nftables.service`, which loads the same file at boot (`Before=network-pre.target`, `After=ufw.service`). The stock `nftables.service` isn't used because it starts with `flush ruleset` and would wipe UFW. `TestNftRules` checks the rules for three agents and checks that a single agent gets no rules.

**Deletion** (`DeleteAgent`) stops the worker socket and service, then runs `disable-linger`, `terminate-user`, `pkill -KILL -u`, `userdel --remove`, and removes the home. With `--keep-home` the home is moved to `/srv/laavu/deleted/<name>-<time>` instead. The slice drop-in and `agents/<name>.toml` are deleted, users' access to the agent is removed with `laavud access drop` (so a new agent with the same name does not inherit it), the nftables rules are rebuilt and `laavud` restarts.

**Limits** (`limitsCmd`) merges the new keys into the existing drop-in, writes it back and applies it right away with `systemctl set-property --runtime`.

## `check-isolation`

`CheckIsolation(a, b)` sets up targets owned by B and runs the probe as A:

- root listens on the first free port in B's range and answers `laavu-probe` (the nftables rule looks at who connects, so it doesn't matter that root is the listener);
- B's file in `/tmp` is created with `sudo -u B`, which goes through the full PAM stack so `pam_umask` applies (`runuser` skips it);
- B's process is `runuser -u B sleep 60`;
- the probe itself is `systemd-run --uid=A --slice=user-<uid A>.slice -p UMask=0077 laavud priv probe JSON`, running in A's slice the same way its sessions do.

| Probe (`probeMain`) | Blocked if |
|---|---|
| read home | `ReadDir` of B's home fails |
| file in home | `Stat` on B's `~/.claude/settings.json` fails |
| processes | `/proc` has no process with B's uid |
| agent port | the connection fails or the answer is not `laavu-probe`; if B's whole range is taken, the probe is marked skipped |
| file in /tmp | B's file cannot be read |
| worker socket | cannot connect to `/run/laavu/workers/lv-B.sock` |
| laavud database | `/var/lib/laavu/laavud.db` cannot be read |
| sudo | `sudo -n true` fails |

Each probe prints as `blocked`, `skipped` or `OPEN`. With no arguments every ordered pair is checked. A summary (time, number of pairs, open probes) goes to `/var/lib/laavu/isolation-check.json`, where the client and `laavu check` read it. If any probe is open, the command exits with an error.

## Server file map

Permissions come from the code and the units. Where umask or systemd sets them instead of the code, the table says so.

| Path | Owner, mode | Created by | Contents |
|---|---|---|---|
| `/usr/local/bin/laavu`, `laavud` | root, 0755 | `install`, `update` | binaries; `.prev` next to them after an update |
| `/etc/laavu/` | root, 0755 | `stepConfig` | `laavud.toml` (0644), `reserved`, `tmux.conf`, `nftables.nft`, `release.pub`, `tailscale.json`, `link.toml` (root:laavu-link 0640, [`link.md`](link.md)) |
| `/etc/laavu/agents/<name>.toml` | root, 0644 | `CreateAgent` | name, user, home, ports, uid |
| `/etc/laavu/agent-template/` | root, 0644 | `stepConfig`, `update templates` | agent rule templates for the owner to edit |
| `/etc/tmpfiles.d/laavu.conf` | root | `stepConfig` | directories in `/run/laavu` |
| `/run/laavu/`, `/run/laavu/workers/` | root:root, 0755 | tmpfiles | worker sockets `<user>.sock`, `laavud:laavud 0600`, created by systemd (`laavud-worker@.socket`) |
| `/run/laavu/http/` | laavud:caddy, 0750 | tmpfiles | `app.sock`, `aux.sock` for Caddy |
| `/run/laavu/hooks/` | laavud:laavu-agents, 0750 | tmpfiles | `hook.sock` for agent hooks |
| `/var/lib/laavu/` | laavud, 0711 | systemd (`StateDirectory=laavu`) | `laavud.db` (mode from the unit's `UMask=0077`), `isolation-check.json` (0644, handed to the directory owner) |
| `/srv/laavu/agents/` | root, 0755 | `stepUsers` | agent homes, each `lv-<name>`, 0700 |
| `/srv/laavu/deleted/` | root, 0700 | `DeleteAgent --keep-home` | kept homes of deleted agents |
| `/var/log/laavu/` | root, 0750 | `stepUsers` | `priv.log` (0640) |
| `/etc/systemd/system/` | root | `install`, `CreateAgent` | `laavud.service`, `laavud-worker@.{socket,service}`, `laavu-nftables.service`, `user-<uid>.slice.d/laavu.conf`; `laavu link` adds `laavu-link.service` and `laavu-link-helper.{socket,service}` |
| `/etc/ssh/sshd_config.d/01-laavu.conf` | root, 0644 | `stepSSH` | `DenyGroups`, password ban |
| `/etc/caddy/Caddyfile` | root, 0644 | `stepCaddy`, `tailscale` | Laavu sites |

The daemon unit is locked down so it can't reach agent homes: `ProtectSystem=strict`, `ProtectHome=yes`, `InaccessiblePaths=-/srv/laavu/agents`, write access only to `/run/laavu/http` and `/run/laavu/hooks`, `PrivateTmp`, `NoNewPrivileges`, an empty capability set and `MemoryMax=512M`. The worker unit has `User=%i`, `UMask=0077`, `NoNewPrivileges` and `MemoryMax=256M` for the worker process itself (details in [`arch-worker.md`](arch-worker.md)).

## Releases

The code is in `internal/release/release.go`. A release is a directory with the binaries (`laavud-linux-amd64` and so on), `manifest.json` (version, time, and name, sha256 and size for each file) and `manifest.json.sig`. The `.sig` file is a base64 Ed25519 signature over the exact bytes of the manifest. A release source can be a directory or an https URL (`Source`), and downloads are capped at 512 MB.

| Command | What it does |
|---|---|
| `laavu release keygen --out FILE` | a key pair; the private key goes to a 0600 file (base64), the public key is printed |
| `laavu release manifest --dir D --version V` | a manifest from the files in the directory |
| `laavu release sign --dir D --key FILE` | sign the manifest |
| `laavu release verify --dir D [--key BASE64] [--file NAME=PATH]` | check the signature and, optionally, file hashes |

**Project key.** The public key is baked in at build time (`-X …/internal/release.ProjectKey`) and pinned to `/etc/laavu/release.pub` during install. `PinnedKey` tries the pinned key first and falls back to the embedded one. The project owner keeps the private key. CI never sees it.

**CI** (`.github/workflows/release.yml`) runs on a `v*` tag. It does `go vet`, `go test`, the client build, `laavu` and `laavud` for linux amd64 and arm64, and `laavu` for darwin amd64, arm64 and windows amd64. The project key comes from the `LAAVU_PROJECT_KEY` variable. The output is an unsigned manifest and a draft release. Signing is done by hand: download `manifest.json`, run `laavu release sign`, upload the `.sig` to the same release and publish it.

**`laavu update laavud [--from …] [--force] [--timeout 20s]`** (`updateLaavud`). The default source is `https://github.com/ilguzin/laavu/releases/latest/download`. It checks the signature first, then compares versions (`release.Newer`, `x.y.z` with an optional `v`). Without `--force` it refuses to install an older or equal version. Then it downloads `laavud` and `laavu` into temporary `.staged` files and checks their hashes, copies the current binaries to `.prev`, swaps in the new ones with `rename`, restarts the daemon and runs `try-restart` on the workers. After that it waits for the daemon to answer on `app.sock` (`GET /manifest.webmanifest` with a status below 500). If there's no answer before the timeout, it puts `.prev` back, restarts, and exits with `update to <version> rolled back, running <version>`.

Tests are `TestSignVerifyFetch` (a tampered binary fails the hash check, and a tampered manifest or a foreign key fails the signature check) and `TestNewer`. On the VM, `test/vm/e2e-ssh.sh` updates 0.0.1 → 0.0.2, checks that a broken 0.0.3 rolls back to 0.0.2, and checks that a release with a foreign signature is rejected.

## Maintenance

| Command | Details |
|---|---|
| `check` | versions of `laavud`, `laavu`, tmux, Caddy, the kernel; state of `laavud`, `caddy`, `laavu-nftables`, `ufw`, `ssh`; disk (problem above 90 %), memory; Caddy certificates except the internal CA (problem under 14 days); workers and the `claude` version per agent; the result of the last `check-isolation` (the check itself is not run); the number of updates from `apt-get -s upgrade`, whether a reboot is needed; whether the daemon responds. `--json` gives a `Report` (schema 1, with the Tailscale mode), part of the console contract in [`link.md`](link.md#report) |
| `firewall` | `ufw status verbose`, `ufw allow\|deny PORT[/tcp]` with a log entry |
| `cleanup` | `journalctl --vacuum-time=30d`, `apt-get clean`, `autoremove`; in agents' `~/tmp`, as the agent (`AsAgent`, `systemd-run --uid` in its slice), `find -P -xdev`, so that a `~/tmp → /` symlink does not turn cleanup into deleting the system. Tested in `e2e-server.sh` |
| `diag` | diagnostics without folder contents |
| `restart laavud` | only the daemon |
| `update os` | `apt-get upgrade` with `--force-confold`, reports if a reboot is needed, does not reboot by itself |
| `update claude` | `claude update` for each agent as that agent; skips agents without `~/.local/bin/claude` |
| `update templates` | `tmux.conf` and the templates in `agent-template/` from the embedded ones, the old template goes to `.bak`; does not touch the rules of existing agents |

`backup snapshot` (a snapshot through the provider API) isn't implemented.

## Tailscale

The code is in `internal/install/tailscale.go`. `laavu tailscale enable [--mode extra|only] [--authkey KEY] [--hostname NAME] [--ssh-port 22]` does this (the work is in `tsSwitch`, shared with `install --tailscale`):

1. installs Tailscale with the official script if it isn't there;
2. runs `tailscale up --ssh=false`, with `--authkey` and `--hostname` if given (without a key, the login link is printed to the terminal);
3. in `only` mode on a server in port mode (VM, `--host`), switches the address to `https://<name>.ts.net` (the name comes from `tailscale status --json`, and the tailnet needs MagicDNS and HTTPS turned on) and the view to `:8444`, writes a Caddyfile without `tls internal` and lets Caddy get certificates from tailscaled (`tailscale set --operator=caddy`), after which passkeys have to be registered again;
4. changes UFW (`FirewallRules`): `extra` allows ssh only from `tailscale0`, and `only` does the same for the app ports;
5. saves the previous addresses to `/etc/laavu/tailscale.json`; when the address moved, restarts `laavud` and prints a registration link if there is no administrator yet, otherwise tells you to run `laavud reset-auth`.

`disable` restores the addresses from `tailscale.json`, writes the test VM Caddyfile for `localhost` and the UFW rules from `install` for ssh port 22, then runs `tailscale down`.

Tested by `TestFirewallRules` (rules for both modes and for disabling), `TestCaddyData` and `TestHostMode` in `tailscale_test.go`, and by `TestSslip`, `TestResolveAddress`, `TestTailnetName` and `TestSshdPasswords` in `address_test.go`. `enable` itself hasn't been run on a server yet. VM access from a tailnet is done by port forwarding from the host (`test/vm/tailnet.sh` and `laavu install --host`). A live check will happen on a real VPS.

## Test VM scripts

| Script | What it checks |
|---|---|
| `test/vm/e2e-server.sh` | installation and reinstallation, two agents, `check-isolation`, sessions with the `claude` stub through hooks, tmux surviving a worker restart and living in `user@<uid>.service`, an agent file in `/tmp` with mode 600, `TMPDIR` and `LAAVU_PORTS`, VM reboot (sessions sleep and come back, isolation, nftables and `hidepid` in place), `cleanup` does not follow a symlink, deleting an agent removes the user and the rule, browser through Caddy |
| `test/vm/e2e-ssh.sh` | `install --ssh` with a password and a signed release: fingerprint, two signature checks, link, key-only login, `release.pub` pinned; a substituted host key is rejected; `update`, rollback of a broken release, a foreign signature |
| `test/vm/e2e-link.sh` | the console link on its own VM through systemd, against `test/linkstub` ([`link.md`](link.md#testing)) |
| `test/vm/tailnet.sh` | not a test: opens the VM to a tailnet by port forwarding and `install --host` |
| `test/vm/stand/*.sh` | test bench experiments before development started ([`stand-vm.md`](stand-vm.md)) |

How to create, roll back and log in to the VM is in [`dev.md`](dev.md).
