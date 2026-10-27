#!/bin/sh
# Laavu installer, served at https://getlaavu.com/install.sh
#
#   curl -fsSL https://getlaavu.com/install.sh | sh
#
# With no arguments it asks a few questions: install on this server or on a remote
# one over ssh, domain (or, without one, a free sslip.io name or your tailnet), email,
# first agent. Any `laavu install` flags skip the
# questions, e.g. on a server:
#   curl -fsSL https://getlaavu.com/install.sh | sh -s -- --domain laavu.example.com --email you@example.com --agent main --install-claude
# or from a laptop:
#   curl -fsSL https://getlaavu.com/install.sh | sh -s -- --ssh root@203.0.113.10 --domain laavu.example.com --email you@example.com
#
# The script downloads the latest release from GitHub, checks the manifest signature
# against the project key below and the file hashes against the manifest, and then
# hands all arguments over to `laavu install`. Read it before running it.
#
# Environment:
#   LAAVU_VERSION       release tag to install, e.g. v0.1.0 (default: latest)
#   LAAVU_RELEASE_URL   where to download from (default: GitHub releases); file:// works for testing
#   LAAVU_DRY_RUN=1     download and verify, print the install command, do not run it

set -eu

# Project public key (ed25519, base64). Release manifests are signed with the matching
# private key, which never leaves the project owner's machine.
PROJECT_KEY="aSeCzyKGds680dhQa7YOWg4yCHc/JG+h4KN/Zkm8rcw="

REPO="https://github.com/ilguzin/laavu"

say() { printf '%s\n' "$*" >&2; }
die() { say "laavu installer: $*"; exit 1; }

if [ -n "${LAAVU_RELEASE_URL:-}" ]; then
	BASE=$LAAVU_RELEASE_URL
elif [ -n "${LAAVU_VERSION:-}" ]; then
	BASE="$REPO/releases/download/$LAAVU_VERSION"
else
	BASE="$REPO/releases/latest/download"
fi

[ -n "$PROJECT_KEY" ] || PROJECT_KEY=${LAAVU_PROJECT_KEY:-}
[ -n "$PROJECT_KEY" ] || die "no release has been published yet. See $REPO for building from source."

remote=""
for a in "$@"; do
	case $a in
	--ssh | --ssh=*) remote=1 ;;
	--release | --release=*) die "--release is set by this script, leave it out" ;;
	esac
done

os=$(uname -s)
case $os in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "unsupported system $os. On Windows, download laavu-windows-amd64.exe from $REPO/releases and run it with --ssh." ;;
esac
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac

tty=""
if [ -r /dev/tty ] && (: </dev/tty) 2>/dev/null; then
	tty=1
fi

# ask QUESTION [DEFAULT] prints the answer read from the terminal.
ask() {
	if [ -n "${2:-}" ]; then
		printf '%s [%s]: ' "$1" "$2" >&2
	else
		printf '%s: ' "$1" >&2
	fi
	IFS= read -r ans </dev/tty || ans=""
	printf '%s' "${ans:-${2:-}}"
}

# yes QUESTION DEFAULT succeeds on yes.
yes() {
	case $(ask "$1" "$2") in [yY]*) return 0 ;; *) return 1 ;; esac
}

if [ $# -eq 0 ]; then
	[ -n "$tty" ] || die "no terminal to ask questions on. Pass laavu install flags, see the top of this script."
	say "Laavu installer. Press Enter to accept the value in brackets."
	say ""
	if [ "$os" = linux ] && [ -f /etc/os-release ] && grep -q '^ID=ubuntu' /etc/os-release && yes "Install Laavu on this machine?" y; then
		:
	else
		target=$(ask "Server to install on, as user@address (root or a user with sudo)")
		[ -n "$target" ] || die "no server given"
		set -- --ssh "$target"
		remote=1
	fi
	domain=$(ask "Domain for Laavu (leave empty if you have none)")
	if [ -z "$domain" ]; then
		say "Passkeys need a host name with HTTPS, a bare IP address will not do. Without a domain:"
		say "  1) a free name from sslip.io, like 203-0-113-10.sslip.io (open from anywhere)"
		say "  2) your Tailscale tailnet (only your devices in it can open Laavu, ssh moves there too)"
		say "  3) enter a domain after all"
		case $(ask "Choose 1, 2 or 3" 1) in
		1) set -- "$@" --sslip ;;
		2)
			say "Turn on MagicDNS and HTTPS certificates in the tailnet's admin console first."
			key=$(ask "Tailscale auth key for the server (admin console, Settings, Keys)")
			[ -n "$key" ] || die "an auth key is needed to add the server to the tailnet"
			name=$(ask "Server name in the tailnet" laavu)
			set -- "$@" --tailscale --tailscale-authkey "$key" --tailscale-hostname "$name"
			;;
		3)
			domain=$(ask "Domain for Laavu")
			[ -n "$domain" ] || die "no domain given, nothing installed"
			;;
		*) die "no way to open Laavu chosen, nothing installed" ;;
		esac
	fi
	if [ -n "$domain" ]; then
		say "Point both $domain and view.$domain at the server before going on."
		email=$(ask "Email for Let's Encrypt")
		[ -n "$email" ] || die "an email is needed for the certificate"
		set -- "$@" --domain "$domain" --email "$email"
	fi
	agent=$(ask "Name of the first agent" main)
	set -- "$@" --agent "$agent"
	if yes "Install Claude Code for it?" y; then
		set -- "$@" --install-claude
	fi
	say ""
fi

sudo=""
if [ -z "$remote" ]; then
	[ "$os" = linux ] || die "to install from this machine onto a server, pass --ssh USER@HOST"
	if [ "$(id -u)" != 0 ]; then
		command -v sudo >/dev/null 2>&1 || die "run this as root"
		sudo=sudo
	fi
fi

for c in curl openssl; do
	command -v "$c" >/dev/null 2>&1 || die "$c is required"
done
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "sha256sum or shasum is required"
fi

dir=$(mktemp -d "${TMPDIR:-/tmp}/laavu-install.XXXXXX")
trap 'rm -rf "$dir"' EXIT INT TERM

fetch() {
	curl -fsSL --proto '=https,file' --retry 3 -o "$dir/$2" "$BASE/$1" || die "could not download $BASE/$1"
}

say "Downloading Laavu from $BASE"
fetch manifest.json manifest.json
fetch manifest.json.sig manifest.json.sig

# Signature: ed25519 over the exact bytes of manifest.json. The raw 32-byte key is
# wrapped into a SubjectPublicKeyInfo (fixed 12-byte prefix) so openssl can read it.
{
	printf '%s\n' '-----BEGIN PUBLIC KEY-----'
	{
		printf '\060\052\060\005\006\003\053\145\160\003\041\000'
		printf '%s' "$PROJECT_KEY" | openssl base64 -d -A
	} | openssl base64 -A
	printf '\n%s\n' '-----END PUBLIC KEY-----'
} >"$dir/project.pem"
openssl base64 -d -A <"$dir/manifest.json.sig" >"$dir/manifest.sig.bin"
if ! openssl pkeyutl -verify -pubin -inkey "$dir/project.pem" -rawin \
	-in "$dir/manifest.json" -sigfile "$dir/manifest.sig.bin" >/dev/null 2>&1; then
	if openssl pkeyutl -help 2>&1 | grep -q -- -rawin; then
		die "release signature does not match the project key. Do not install this release."
	fi
	die "this openssl ($(openssl version)) cannot check ed25519 signatures. Install OpenSSL 3 (on macOS: brew install openssl) and put it first in PATH."
fi

version=$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$dir/manifest.json" | head -n 1)

# expected NAME prints the sha256 the manifest lists for NAME.
expected() {
	awk -v want="$1" '
		/"name":/   { n = $0; sub(/.*"name": *"/, "", n); sub(/".*/, "", n) }
		/"sha256":/ { if (n == want) { h = $0; sub(/.*"sha256": *"/, "", h); sub(/".*/, "", h); print h; exit } }
	' "$dir/manifest.json"
}

get() {
	want=$(expected "$1")
	[ -n "$want" ] || die "release $version has no $1"
	fetch "$1" "$2"
	[ "$(sha256 "$dir/$2")" = "$want" ] || die "$1 does not match the signed manifest. Do not install this release."
	chmod 755 "$dir/$2"
}

if [ -n "$remote" ]; then
	get "laavu-$os-$arch" laavu
	# laavu reads a release from an https URL or a directory.
	set -- install --release "${BASE#file://}" "$@"
else
	get "laavu-linux-$arch" laavu
	get "laavud-linux-$arch" laavud
	set -- install "$@"
fi
say "Laavu $version: signature and hashes verified."

if [ -n "${LAAVU_DRY_RUN:-}" ]; then
	say "Dry run, would now run: ${sudo:+sudo }$dir/laavu $*"
	exit 0
fi

# Under `curl | sh` stdin is the script itself. laavu may ask questions (host key
# fingerprint, ssh password), so give it the terminal.
if [ -n "$tty" ]; then
	$sudo "$dir/laavu" "$@" </dev/tty
else
	$sudo "$dir/laavu" "$@"
fi
