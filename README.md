# Laavu

[![ci](https://github.com/ilguzin/laavu/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ilguzin/laavu/actions/workflows/ci.yml)
[![release build](https://github.com/ilguzin/laavu/actions/workflows/release.yml/badge.svg)](https://github.com/ilguzin/laavu/actions/workflows/release.yml)
[![latest release](https://img.shields.io/github/v/release/ilguzin/laavu?include_prereleases&sort=semver)](https://github.com/ilguzin/laavu/releases)
[![license](https://img.shields.io/badge/license-AGPL--3.0-blue)](LICENSE)

> [!WARNING]
> **Early stage, use at your own risk.** Laavu is young and changes quickly: things break, and releases may change behavior without notice. The installer runs as root and sets up system users, services and firewall rules on your server. Laavu is provided "as is", with no warranty of any kind and no liability for damage or lost data (sections 15 and 16 of the [license](LICENSE)). You are responsible for your server, what your agents do on it, and your backups. Start on a server you can rebuild.

Long-lived work sessions on your own server. Install Laavu on a VPS or your own hardware and work with CLI agents (Claude Code and others), terminals and files from your laptop and phone. Sessions run in tmux: close the browser and the agent keeps working, open it from another device and you are back in the same session.

The name is Finnish for an open lean-to shelter in the forest. Anyone may use one, and the travelers themselves keep it in order.

> A Russian UI language pack is planned.

## Features

- Agent and terminal sessions in tmux on the server. They survive closing the browser and restarting the daemon, and after a server reboot they come back from a snapshot (`claude --resume`).
- Session status from Claude Code hooks: "working", "waiting for reply", the agent's last action. When the agent is waiting for you, your phone gets a push notification.
- New sessions in an existing workspace or in a fresh `git clone`. Each agent can have its own ssh key for that, created from the browser.
- Files next to the terminal: tree view, Markdown, PDF, image and HTML preview, editing with version check, git-based "what changed" view, uploads from the phone.
- The phone is a first-class client: PWA, extra key bar, swipe scrolling.
- Passkey sign-in, no passwords.
- Agent isolation: each agent gets its own system user, systemd limits, its own ports and nftables rules. The daemon runs without root.
- One-command install on the server or from your laptop over ssh. Updates check the signature and roll back on failure.

## How it works

```
browser ── HTTPS ── Caddy ── laavud (daemon) ── laavud worker (one per agent) ── tmux ── claude, shell
```

`laavud` contains the daemon, the agent worker and the Claude Code and tmux hooks. The client (Preact, xterm.js, CodeMirror) is embedded in the same binary.

`laavu` is what the server owner runs to install, manage agents, update, check isolation and collect diagnostics.

Details in [`docs/architecture.md`](docs/architecture.md).

## Install

On a fresh Ubuntu 24.04 server, or on your Mac or Linux laptop to install onto a server over ssh:

```sh
curl -fsSL https://getlaavu.com/install.sh | sh
```

It asks where to install and the domain. Without a domain it offers a free `sslip.io` name for the server's IP or your Tailscale tailnet. Then it checks the release signature and runs the installer. The script is [`install.sh`](install.sh) in this repository, read it before running it. At the end you get a link to register the first passkey. Flags for scripted installs, DNS, signing in Claude Code, adding a phone and running without a domain are in [`docs/install.md`](docs/install.md). Installing from a web console is planned.

## Docs

To set up a server, read [`docs/install.md`](docs/install.md). For how the code works, start with [`docs/architecture.md`](docs/architecture.md). It covers the processes, who talks to whom and where state lives.

| Document | About |
|---|---|
| [`docs/install.md`](docs/install.md) | installing and running a server |
| [`docs/arch-daemon.md`](docs/arch-daemon.md) | the daemon: HTTP and WebSocket, session status, sign-in, push, file preview and dev servers |
| [`docs/arch-worker.md`](docs/arch-worker.md) | the agent worker: protocol with the daemon, tmux, files, hooks |
| [`docs/arch-client.md`](docs/arch-client.md) | the web client: tabs, terminal, editor, phone |
| [`docs/arch-laavu.md`](docs/arch-laavu.md) | the `laavu` tool: install, agent isolation, releases, maintenance |
| [`docs/link.md`](docs/link.md) | the contract with the management console: rights, protocol, report, tests |
| [`docs/dev.md`](docs/dev.md) | build, tests, dev mode, test VM, releases |
| [`docs/stand-claude-code.md`](docs/stand-claude-code.md), [`docs/stand-vm.md`](docs/stand-vm.md) | how Claude Code, tmux and the isolation measures behaved on a test stand |

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for how to send a fix. Please don't open issues for vulnerabilities. Report them privately as described in [`SECURITY.md`](SECURITY.md).

## License

[GNU AGPL v3](LICENSE). If you run a modified version of Laavu as a service for other people, they are entitled to its source code.
