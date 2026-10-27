# Security

Laavu gives access to terminals and files on a server. Please report vulnerabilities privately and keep them out of public issues.

## How to report

Use [GitHub private vulnerability reporting](https://github.com/ilguzin/laavu/security/advisories/new) (Security tab → Report a vulnerability). Describe what an attacker can do, under what conditions, and how to reproduce it. If you have an idea for a fix, include it.

We respond within a week. Please don't publish details until a fix is out. We will credit you in the release notes unless you'd rather not be named.

## What counts as a vulnerability

We are most interested in anything that breaks the boundaries the security model relies on (see [`docs/architecture.md`](docs/architecture.md) and [`docs/arch-laavu.md`](docs/arch-laavu.md)):

- an agent reads or modifies files, processes, sessions or ports of another agent or of the server owner
- an agent gains root, the privileges of the `laavud` daemon or access to its database
- sign-in without a passkey, getting past the agent access check, stealing a sign-in session
- code from a previewed HTML file or from an agent's dev server acting on the user's behalf in the main app
- escaping the working directory in file operations
- installing or updating without verifying the release signature

## What doesn't

- An agent forging the status or last action of its own session. Agents are allowed to do that.
- Anything an agent does within its own user and working directory, including with data the owner gave it.
- Data sent to the model provider. That is up to the server owner.
- Vulnerabilities in Claude Code, tmux, Caddy and other external programs. Report those to their authors. If Laavu configures them insecurely, though, that one is ours.

## Supported versions

For now we only fix the latest release.
