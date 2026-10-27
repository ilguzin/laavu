# Contributing

Thanks for wanting to help. Laavu is at an early stage, so please open an issue before starting anything large. The plan for that part may be different from what the code suggests.

## What helps

Bugs go into an issue using the template: version (`laavu version`), server OS, browser and device, steps, what you expected and what you got. Output of `sudo laavu diag` helps a lot. Look through it before pasting, though, since it contains agent names and the server address.

For a feature request, open an issue and describe the problem you have. A proposed solution alone is hard to judge.

Fixes come as pull requests. Small ones (typos, docs, obvious bugs) can be sent right away.

Vulnerabilities don't go into issues. See [`SECURITY.md`](SECURITY.md).

## Environment

Build, tests, dev mode and the test VM are covered in [`docs/dev.md`](docs/dev.md). The everyday commands:

```sh
make web build   # client and binaries in bin/
make test        # go vet and go test, needs tmux and git
make e2e         # Playwright against laavud dev
```

`bin/laavud dev` runs the daemon and worker in one process as the current user, without root and without sign-in, at `http://localhost:7700`.

## Pull requests

- One change per PR. Keep refactoring separate from behavior changes.
- `make test` passes, and `make e2e` as well if you changed the client or the protocol.
- New behavior is covered by a test: a Go test next to the code or a scenario in `test/e2e/tests`. The worker and daemon are tested against real tmux, with the `test/fakeclaude` stub in place of Claude Code.
- If you changed installation, isolation or systemd units, run `make vm-e2e` on the test VM or say in the PR that you didn't.
- Code is formatted with `gofmt`, and the client passes `tsc --noEmit` (`npm run build` in `web/`).
- If behavior changes noticeably, update the document in `docs/` that describes it.

## Style

- Follow the surrounding code. Comments, docs and identifiers are in English.
- No new dependencies without a real need: the server is a single static binary without cgo, and the client must stay light enough for a phone.
- The daemon doesn't get root and doesn't store anything it can get from tmux, the agent or the file system.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/): `fix(worker): …`, `feat(web): …`, `docs: …`.

## License of contributions

By submitting changes you agree that they are distributed under the project's license, [GNU AGPL v3](LICENSE).

## Code of conduct

The project follows [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).
