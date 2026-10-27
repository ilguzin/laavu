# Development

Code structure is in [`architecture.md`](architecture.md).

## What you need

- Go 1.26+ (if Go is installed in `~/.local/opt/go`, the `Makefile` adds it to `PATH` itself).
- Node 22+ for the client and Playwright.
- tmux and git for the worker and daemon tests.
- VirtualBox and docker for the test VM (docker is only needed to build the cloud-init ISO).
- Chromium for Playwright: `cd test/e2e && npx playwright install chromium`. If the system lacks libraries and there is no sudo, the workaround is in [`test/e2e/README.md`](../test/e2e/README.md).

## Build

```sh
make web     # client: web/ → web/dist → internal/webui/dist
make build   # bin/laavu and bin/laavud, client embedded in laavud
```

Without `make web` the daemon still builds, but it answers "client is not built" where the client should be.

## Tests

```sh
make test    # go vet and go test ./... — worker and daemon on real tmux, with a claude stub
make e2e     # Playwright against laavud dev: sessions, files, view, ssh key and clone, passkey, phone
test/installer.sh   # install.sh in dry-run mode against a test release signed with a throwaway key
```

`install.sh` in the repository root is the script getlaavu.com serves. It depends on the manifest format, the release file names and the `laavu install` flags, so it lives here and `test/installer.sh` runs in CI on every push: shellcheck, install on the machine and over `--ssh`, refusal of a tampered release, and the flags it passes. The site deploy ships `install.sh` only from a commit on `main` and runs the check with `--published` first, against the latest published release too.

The `test/fakeclaude` stub calls hooks from `~/.claude/settings.json` and `--settings` with the same fields as the real Claude Code (we checked them on a test stand, see [`stand-claude-code.md`](stand-claude-code.md)). It writes a transcript with counters and understands `/clear`, `/compact` and `/exit`. Send it `write:path` and it writes that file as if through the Write tool.

## Client

The client lives in `web/src`. It is Preact with no state library (`store.ts`). The theme is a set of tokens on `:root[data-theme]` in `style.css`, and `theme.ts` picks the theme and builds the xterm theme from the same tokens.

- Layout: the agent rail (`Rail`), the session list (`Sidebar`), tab groups (`Workspace`), the file navigator (`Navigator`). On a phone, one screen at a time and a bottom menu.
- A tab is a session (`SessionView`: header, xterm, and on a phone the `KeyBar` key strip) or a file (`FileView`: view, source, edit, changes). Tab groups 1–4 are stored in `localStorage`.
- There is no separate input field. You type straight into the terminal. After an upload the path is inserted there without Enter, and so is a quote from a file.
- The editor is CodeMirror 6 (`editor/cm.ts`), a separate chunk. Saving is `PUT …/files/{folder}/file` with the version from the read. If the file on disk has a different version, the server returns `409` and the client shows a comparison.

## Development mode

```sh
bin/laavud dev --home /tmp/laavu-home --claude "$(pwd)/bin/fakeclaude"   # no sign-in, http://localhost:7700
bin/laavud dev --home /tmp/laavu-home --auth                             # with passkey, registration link in the output
```

The daemon and the worker run in one process as the current user. The view origin is on `:7701`, dev servers on `:7710–7719`. The agent gets its own home folder, your real home isn't used.

## Test VM

```sh
test/vm/create.sh        # VM from the Ubuntu 24.04 cloud image, snapshot clean
test/vm/reset.sh         # roll back to clean (about 30 s)
test/vm/ssh.sh           # log in to the VM
test/vm/e2e-server.sh    # installation, two agents, isolation, sessions, reboot, browser through Caddy
test/vm/e2e-ssh.sh       # laavu install --ssh with a password, signed releases, update and rollback
```

`test/vm/e2e-link.sh` checks the console link against `test/linkstub` on a separate VM, see [`link.md`](link.md#testing).

VM ports are forwarded to the host one to one: the app at `https://localhost:8443`, the view origin at `:8444`, dev servers at `:8450–8459`, ssh at `:2222`. The certificate comes from Caddy's internal CA, and the browser on the host complains about it until the CA root (`/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt` on the VM) is added to the trusted roots. Passkeys with RP ID `localhost` work.

**Access from a tailnet.** `test/vm/tailnet.sh` forwards the same ports to the host's tailnet address and switches Laavu on the VM to the host name (`laavu install --host HOST.<tailnet>.ts.net`). Your machine has to trust the Caddy CA root once (the script puts it in `~/laavu-test-root.crt` on the host). Without a trusted certificate the browser turns passkeys off. After `reset.sh`, run the script again.

By hand on the VM:

```sh
test/vm/reset.sh
scp -P 2222 bin/laavu bin/laavud ubuntu@127.0.0.1:/tmp/
test/vm/ssh.sh 'sudo /tmp/laavu install --host localhost --agent demo --install-claude'
```

or from the host with one command, the way a VPS owner will do it:

```sh
bin/laavu install --ssh ubuntu@127.0.0.1:2222 --host localhost --agent demo --install-claude
```

Without `--release` it installs an unsigned local build and prints a warning about it.

## Releases

The project owner keeps the signing key. CI never has it.

Once:

```sh
(umask 077 && mkdir -p ~/.config/laavu && openssl genpkey -algorithm ed25519 -out ~/.config/laavu/release.pem)
openssl pkey -in ~/.config/laavu/release.pem -pubout -outform DER | tail -c 32 | base64   # the public key
gh variable set LAAVU_PROJECT_KEY --body PUBLIC_KEY                                        # CI embeds it into the binaries
```

`laavu release sign` reads this PEM file as well as the base64 one from `laavu release keygen` (tests use that). Keep a backup of `release.pem` offline: servers that are already installed accept only releases signed with it.

The same public key goes into `PROJECT_KEY` in `install.sh`. A new release has to install with the `install.sh` that getlaavu.com serves at the time: change the manifest, file names or install flags so that the old script still works, or deploy the site with a script that handles both before the release.

Each release starts from a commit on `main` with green CI:

```sh
git tag -a v0.1.0 -m "Laavu 0.1.0" && git push origin v0.1.0   # the release workflow builds a draft
gh release download v0.1.0 --dir dist
bin/laavu release sign --dir dist --key ~/.config/laavu/release.pem
bin/laavu release verify --dir dist --key PUBLIC_KEY --file laavud-linux-amd64=dist/laavud-linux-amd64   # and the other files
gh release upload v0.1.0 dist/manifest.json.sig
gh release edit v0.1.0 --draft=false --latest --notes-file NOTES.md
```

A version with a suffix (`v0.2.0-rc.1`) becomes a prerelease. `releases/latest` skips it, so `install.sh` and `laavu update laavud` install it only when pointed at it: `LAAVU_VERSION=v0.2.0-rc.1` for the first, `--from https://github.com/ilguzin/laavu/releases/download/v0.2.0-rc.1` for the second.

The server checks the signature with the key pinned at installation (`/etc/laavu/release.pub`) and rolls back if `laavud` does not respond after an update.

## Service commands

- `laavud call AGENT OPERATION [JSON]` sends a request straight to an agent's worker (as `laavud` or root), for diagnostics: `sudo runuser -u laavud -- laavud call demo sessions`.
- `laavu diag` shows services, logs, rules and sockets. Folder contents are left out.
- `laavu check --json` prints a status report. Its schema is part of the console contract, see [`link.md`](link.md#report).
