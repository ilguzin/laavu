# Client end-to-end tests

`make e2e` from the repo root builds the client, `laavud` and the `test/fakeclaude` stub, then runs Playwright against two `laavud dev` instances: `:7700` without sign-in and `:7800` with passkey (the registration link is taken from `.server-7800.log`). Each server gets its own agent home and its own tmux socket directory in `.state-<port>/`.

Browser: `npx playwright install chromium`. On a host without sudo, the missing Chromium system libraries (libnss3, libnspr4, libatk and others) are unpacked from `.deb` files without installing them:

```sh
mkdir -p ~/.local/opt/pw-libs/debs && cd ~/.local/opt/pw-libs/debs
apt-get download libnspr4 libnss3 libatk1.0-0t64 libatk-bridge2.0-0t64 libxcomposite1 libxdamage1 \
  libxfixes3 libxrandr2 libasound2t64 libatspi2.0-0t64 libxrender1 libxi6 libxres1
for d in *.deb; do dpkg-deb -x "$d" ..; done
```

The config picks up `~/.local/opt/pw-libs` on its own.

`teardown.ts` shuts down the dev tmux servers, which outlive `laavud dev`.

`playwright.vm.config.ts` runs `vm/vm.spec.ts` against a server installed on the test VM, through Caddy and TLS from its internal CA. `test/vm/e2e-server.sh` runs it at the end. By hand, with the link `test/vm/e2e-ssh.sh` saved: `LAAVU_LINK=$(cat ../vm/.last-register-link) npx playwright test -c playwright.vm.config.ts`.
