# Installing Laavu

This guide sets up Laavu on a server you control and gets you to a working agent session in the browser. Plan for about 15 minutes, most of it waiting for packages and the certificate.

## What you need

- A server running Ubuntu 24.04 or newer, amd64 or arm64. A small VPS works: 2 CPU and 2 GB of RAM is enough for one or two agents. A fresh server is best, because the installer changes ssh, the firewall and Caddy.
- ssh access as root or as a user with `sudo`. A password is fine for the first login. The installer switches you to key login.
- A domain, or one of the two ways to do without one (see [Without a domain](#without-a-domain)). With your own domain, point two DNS A records (or AAAA for IPv6) at the server before you start: `laavu.example.com` and `view.laavu.example.com`. The second one is where Laavu shows HTML files and previews, kept on a separate origin from the app. You also need an email address for Let's Encrypt.
- A browser that supports passkeys: any current Chrome, Safari, Firefox or Edge. Laavu has no passwords. You sign in with a passkey stored on your laptop, phone or password manager.

Laavu doesn't work on a bare IP address: passkeys need a host name with HTTPS. Without a domain the installer offers a free `sslip.io` name for the server's IP or your Tailscale tailnet, see [Without a domain](#without-a-domain).

## Three ways to install

The short version, on the server or on your laptop:

```sh
curl -fsSL https://getlaavu.com/install.sh | sh
```

The script asks whether to install on this machine or on a remote server, then the domain and an email (or, without a domain, `sslip.io` or a tailnet) and the name of the first agent. It downloads the latest release, checks its signature against the project key written into the script and runs `laavu install`. [The script](https://getlaavu.com/install.sh) is short, read it before piping it to a shell if you like.

1. [From your laptop](#from-your-laptop). Run it on your Mac or Linux laptop and give it the server's address with the root (or sudo user) password. Everything on the server happens over ssh.
2. [On the server](#on-the-server). Log into the server and run it there.
3. From the Laavu console. Not available yet: for now the console only looks after a server you installed yourself, see [Or let the console do it](#or-let-the-console-do-it).

Both working options end the same way: a registration link for the first passkey.

## From your laptop

```sh
curl -fsSL https://getlaavu.com/install.sh | sh
```

Answer "n" to "Install Laavu on this machine?" and give the server as `root@203.0.113.10`. To skip the questions, pass the flags yourself:

```sh
curl -fsSL https://getlaavu.com/install.sh | sh -s -- --ssh root@203.0.113.10 \
  --domain laavu.example.com --email you@example.com --agent main --install-claude
```

What happens:

1. On the first connection `laavu` shows the server's key fingerprint and asks you to confirm. Compare it with the one in your hosting panel if the panel shows it.
2. It logs in with `~/.ssh/id_ed25519` if you have one and asks for the password otherwise. Your public key goes onto the server. If there's no `~/.ssh/id_ed25519.pub`, it creates a new key at `~/.ssh/laavu_<host>`.
3. It uploads the Linux binaries for the server's architecture and checks the release signature again on the server.
4. It runs the installer on the server. You'll see the steps go by: packages, users, firewall, Caddy, the daemon, the first agent.
5. It checks that key login works and only then turns off password login for ssh.

At the end you get two things: the `ssh -i …` command for getting back onto the server, and a registration link. Keep both.

If you log in as a regular user instead of root, it needs passwordless `sudo`.

On Windows there's no script. Download `laavu-windows-amd64.exe` from the [releases page](https://github.com/ilguzin/laavu/releases) and run:

```sh
laavu-windows-amd64.exe install --ssh root@203.0.113.10 --release https://github.com/ilguzin/laavu/releases/latest/download --domain laavu.example.com --email you@example.com --agent main
```

## On the server

```sh
curl -fsSL https://getlaavu.com/install.sh | sh
```

Run it as root or as a user with `sudo`; the script calls `sudo` itself when it needs to. The installer copies `laavu` and `laavud` to `/usr/local/bin`. The registration link is at the end of the output.

Password login for ssh stays on in this mode. To switch to keys, pass `--owner-key "ssh-ed25519 AAAA…"` with your public key and `--harden-ssh`. The installer won't turn passwords off without a key, so you can't lock yourself out.

If you'd rather not pipe anything into a shell, do the same by hand (`uname -m`: `x86_64` is amd64, `aarch64` is arm64):

```sh
R=https://github.com/ilguzin/laavu/releases/latest/download
curl -fLo laavu  $R/laavu-linux-amd64
curl -fLo laavud $R/laavud-linux-amd64
chmod +x laavu laavud
./laavu release verify --dir $R --file laavu-linux-amd64=laavu --file laavud-linux-amd64=laavud
sudo ./laavu install --domain laavu.example.com --email you@example.com --agent main --install-claude
```

The `verify` line uses the key built into the `laavu` you just downloaded, so it only proves the files belong together. The script is stronger here because it carries the key separately.

## First sign-in

Open the registration link from the install output. It is single-use and valid for 24 hours. Pick a name, create a passkey, and you're in as the admin.

Lost the link? On the server:

```sh
sudo laavud invite --admin
```

If an admin already exists and you lost access, `sudo laavud reset-auth` removes all passkeys and prints a new admin link.

To invite someone else, give them a member link with the agents they may use:

```sh
sudo laavud invite --agent demo
```

Without `--agent` the only agent is granted, or the command asks which ones if there are several. A member sees only these agents. Change it later with `sudo laavud access grant|revoke USER AGENT` and check with `sudo laavud access list`.

To add your phone, open the menu in the left rail and choose **Add device**. Open that link on the phone within 15 minutes and create a passkey there. On the phone you can also add Laavu to the home screen. It works as an app, and push notifications on iOS only work that way.

## Sign Claude Code in

`--install-claude` installs Claude Code for the agent, but the agent still has to be signed in. In Laavu, open a terminal in any of the agent's workspaces (the terminal icon next to the workspace) and run `claude`. It will walk you through sign-in. If you use a token or an API key instead, put it in the agent's `~/.bashrc` from that same terminal:

```sh
echo 'export ANTHROPIC_API_KEY=sk-ant-…' >> ~/.bashrc
```

Every agent is a separate Linux user, so each one is signed in separately and can use a different account or provider.

## More agents

```sh
sudo laavu agent create research --install-claude
sudo laavu agent list
```

Each agent gets its own user, home directory, port range and memory limit (by default a share of the server's memory, change it with `--memory 2G` or later with `laavu agent limits`). Agents can't see each other's files or processes. To check:

```sh
sudo laavu check-isolation
```

`sudo laavu agent delete NAME` removes the agent, its home and everyone's access to it. Add `--keep-home` to keep the files.

## Without a domain

If you answer the domain question with nothing, the script offers two ways to do without one and installs nothing unless you pick one.

### A name from sslip.io

The default. [sslip.io](https://sslip.io) is a public DNS service: `203-0-113-10.sslip.io` and anything under it, such as `view.203-0-113-10.sslip.io`, resolve to `203.0.113.10`. Laavu takes that name for the server's IP and installs as with a domain, with a Let's Encrypt certificate, so it opens from any device. No email is needed.

```sh
curl -fsSL https://getlaavu.com/install.sh | sh -s -- --ssh root@203.0.113.10 --sslip --agent main --install-claude
```

The server needs a public IPv4 address with ports 80 and 443 open. If it sits behind NAT and only sees a private address, pass the public one: `--public-ip 203.0.113.10` (with `--ssh` to a public IP this happens on its own). The name depends on someone else's service, and the Let's Encrypt limit for sslip.io is shared by everyone using it. When you get a domain, run `laavu install --domain … --email …` again and register passkeys anew.

### Your tailnet

Laavu and ssh become reachable only from devices in your [Tailscale](https://tailscale.com) tailnet, at `https://NAME.<tailnet>.ts.net`, with a certificate from Tailscale. In the tailnet's admin console turn on MagicDNS and HTTPS certificates and create an auth key for the server (Settings, Keys). The script asks for the key and the server's name in the tailnet:

```sh
curl -fsSL https://getlaavu.com/install.sh | sh -s -- --ssh root@203.0.113.10 \
  --tailscale --tailscale-authkey tskey-auth-… --tailscale-hostname laavu --agent main
```

The installer doesn't move a server into the tailnet while ssh still takes passwords. From your laptop with `--ssh` it sets up key login and turns passwords off first, then switches. On the server itself, pass your key with `--owner-key "ssh-ed25519 AAAA…" --harden-ssh`, otherwise it stops and says so. After the switch, log in with ssh at the ts.net name from a device in the tailnet. `sudo laavu tailscale disable` opens ssh and the ports to the internet again, but the app goes back to the test VM address `https://localhost:8443`. To open it from outside, run `laavu install --sslip` or `--domain … --email …` after that and register passkeys anew.

### Test VM

For a VM on your own machine there is still `--host NAME` (for example `--host localhost`). Laavu then runs at `https://NAME:8443`, previews at `:8444`, and agents' dev servers at `:8450-8459`. The name has to resolve on your devices, and the certificate comes from Caddy's own CA: browsers disable passkeys until you add its root (`/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt` on the server) to the trusted certificates. Passkeys are tied to the name, so a different `--host` later means registering again.

### Tailscale on a server with a domain

```sh
sudo laavu tailscale enable --mode extra
```

`--mode extra` keeps the app public and only moves ssh into the tailnet. `--mode only` also closes the app to the internet, and the domain keeps its address: point its DNS at the server's tailnet IP, or the app becomes unreachable. Without `--authkey` it prints a login link; `--hostname` sets the server's name in the tailnet. `sudo laavu tailscale disable` restores the previous setup.

## Keeping it running

```sh
sudo laavu check            # disk, memory, certificate, agents, pending OS updates
sudo laavu update laavud    # latest signed release, rolls back if the new daemon doesn't start
sudo laavu update os        # apt upgrade
sudo laavu update claude    # Claude Code for all agents
sudo laavu cleanup          # old logs and agents' temp files
```

`laavu update laavud` only accepts releases signed with the key pinned at install time.

### Or let the console do it

The [Laavu console](https://console.getlaavu.com) can run these on a schedule in a maintenance window, update `laavud` when a release is out and mail you when something breaks. It is optional and in free beta, one server per account. Sign up in the console, press "Link a server" and run on the server:

```sh
sudo laavu link
```

`laavu link` prints a code and the server key fingerprint. Type the code in the console and check that it shows the same fingerprint. Or let the console give you a code first and run the command with `--code`.

The console only gets the rights the server gives it, and it never sees your agents' files or sessions. `sudo laavu link rights` shows and changes them, `sudo laavu unlink` takes control back at any time. The details are in [`link.md`](link.md).

## When something breaks

```sh
sudo laavu diag
```

It prints service status, recent logs, firewall and nftables rules and the config. Read it before pasting it anywhere: it includes agent names and the server address. Then open an issue with it. Security problems go through [`SECURITY.md`](../SECURITY.md) instead.

Things that come up often:

- **The certificate doesn't arrive.** Both DNS records have to point at the server and ports 80 and 443 have to be reachable. `sudo journalctl -u caddy` shows what Let's Encrypt said.
- **The browser won't create a passkey.** The page has to be on HTTPS with a certificate the browser trusts, and you have to use the same address the server was installed with.
- **The agent says it isn't logged in.** See [Sign Claude Code in](#sign-claude-code-in). The token has to be in the agent's `~/.bashrc`, not yours.
- **`laavu update laavud` says the release signature does not match the project key.** See below.

### The release signature does not match

Your server keeps the project's public key in `/etc/laavu/release.pub`, written there at install time. Every update has to be signed by the matching private key, and a release that isn't gets refused. That's the point of signing, so don't work around it by editing the file to whatever key the new release came with.

There are two reasons you can see this error:

1. Something between you and GitHub changed the download. Try again later or from another network. If it keeps failing and nothing was announced, open a security report as described in [`SECURITY.md`](../SECURITY.md).
2. The project replaced its signing key because the old one was lost or leaked. The project announces this in the notes of the first release signed with the new key and on [getlaavu.com](https://getlaavu.com), with the new key in both.

If a key change was announced, check the new key in two places. Compare the key in the release notes with `PROJECT_KEY` in `https://getlaavu.com/install.sh`. They have to be identical. Then pin it and update:

```sh
cat /etc/laavu/release.pub                          # the old key, keep a note of it
echo 'NEW_PUBLIC_KEY' | sudo tee /etc/laavu/release.pub
sudo laavu update laavud
```

If the old key leaked, also look at what you installed between the leak and the announcement, which the notes will date. Run `laavud version` and compare with the releases listed on GitHub. A version you can't find there didn't come from the project. Treat that server as compromised and set up a fresh one.

Reinstalling doesn't replace `release.pub` if it's already there. On a fresh server the installer pins whatever key it was downloaded with.
