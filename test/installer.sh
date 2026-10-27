#!/usr/bin/env bash
# Checks install.sh, the script getlaavu.com serves, against releases in dry-run mode.
#
#   test/installer.sh [--published] [INSTALL_SH]     INSTALL_SH defaults to install.sh here
#
# Always: shellcheck (when installed), then a test release built from this checkout and signed
# with a throwaway key. The script has to install it on this machine and over --ssh and refuse it
# once a binary is tampered with, and the built laavu has to know every flag the script passes to
# `laavu install`. That catches a change here (manifest, file names, flags) that breaks the script.
# With --published the published laavu has to know those flags as well.
#
# --published: also the latest published release, checked with the PROJECT_KEY in the script,
# so a new script does not break installing what is already out. Skipped while the key is empty
# or nothing is published yet.
#
# CI runs it on every push. The site deploy runs it with --published before shipping the script.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
published=""
[[ "${1:-}" != --published ]] || { published=1; shift; }
script="$(realpath "${1:-$root/install.sh}")"

fail() { echo "installer check: $*" >&2; exit 1; }
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

if command -v shellcheck >/dev/null; then
  shellcheck -s sh "$script" || fail "shellcheck"
else
  echo "installer check: no shellcheck, skipping it" >&2
fi

arch=amd64
case "$(uname -m)" in aarch64 | arm64) arch=arm64 ;; esac

# check_flags LAAVU WHAT: the binary knows every flag the script passes to `laavu install`
# (taken from its `set --` lines, so a new question in the script is checked too).
check_flags() {
  local usage f
  usage="$("$1" install -h 2>&1 || true)"
  for f in $(grep -E '^[[:space:]]*(.*\) )?set -- ' "$script" | grep -oE -- '--[a-z][a-z-]*' | sed 's/^--//' | sort -u); do
    grep -q -- "^  -$f\b" <<<"$usage" || fail "$2 has no --$f, which install.sh passes to laavu install"
  done
}

# run ARGS... runs the script quietly and shows its output only on failure.
run() { sh "$1" "${@:2}" >"$work/out" 2>&1 || { cat "$work/out" >&2; return 1; }; }

# The test release is signed with a throwaway key, so check a copy with an empty PROJECT_KEY
# that takes the key from LAAVU_PROJECT_KEY.
sed 's/^PROJECT_KEY=".*"$/PROJECT_KEY=""/' "$script" >"$work/install.sh"
rel="$work/rel"
mkdir -p "$rel"
(
  cd "$root"
  go run ./cmd/laavu release keygen --out "$work/test.key" | tail -n 1 | awk '{print $NF}' >"$work/test.pub"
  for c in laavu laavud; do
    CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$rel/$c-linux-$arch" "./cmd/$c"
  done
  go run ./cmd/laavu release manifest --dir "$rel" --version 0.0.0-test >/dev/null
  go run ./cmd/laavu release sign --dir "$rel" --key "$work/test.key" >/dev/null
) || fail "could not build a test release"

check_flags "$rel/laavu-linux-$arch" "laavu from this checkout"

(
  export LAAVU_DRY_RUN=1 LAAVU_RELEASE_URL="file://$rel" LAAVU_PROJECT_KEY="$(cat "$work/test.pub")"
  run "$work/install.sh" --domain laavu.example.com --email you@example.com --agent main || fail "install on this machine"
  run "$work/install.sh" --ssh root@203.0.113.10 --domain laavu.example.com || fail "install over --ssh"
  echo x >>"$rel/laavud-linux-$arch"
  if run "$work/install.sh" --domain laavu.example.com 2>/dev/null; then fail "a tampered release was accepted"; fi
)
echo "installer check: test release from $(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo this checkout): ok" >&2

[[ -n "$published" ]] || exit 0
if grep -q '^PROJECT_KEY=""$' "$script"; then
  echo "installer check: PROJECT_KEY is empty, skipping the published release" >&2
  exit 0
fi
# Between setting the key and the first release there is nothing published to check.
repo="$(sed -n 's/^REPO="\(.*\)"$/\1/p' "$script")"
if [[ "$(curl -s -o /dev/null -w '%{http_code}' -L "$repo/releases/latest/download/manifest.json")" == 404 ]]; then
  echo "installer check: no published release yet, skipping it" >&2
  exit 0
fi
LAAVU_DRY_RUN=1 run "$script" --ssh root@203.0.113.10 --domain laavu.example.com || fail "the latest published release does not install"
grep -o 'Laavu [^:]*: signature and hashes verified' "$work/out" | sed 's/^/installer check: published /' >&2
# The answers to the script's questions become flags, and the published laavu must know them all:
# a script newer than the release would otherwise install nothing. The binary is checked
# against the manifest the dry run has just verified.
latest="$repo/releases/latest/download"
curl -fsSL -o "$work/manifest.json" "$latest/manifest.json" || fail "could not download the published manifest"
curl -fsSL -o "$work/laavu" "$latest/laavu-linux-$arch" || fail "could not download the published laavu"
want="$(python3 -c 'import json,sys; print(next(f["sha256"] for f in json.load(open(sys.argv[1]))["files"] if f["name"] == sys.argv[2]))' "$work/manifest.json" "laavu-linux-$arch")"
[[ "$(sha256sum "$work/laavu" | cut -d' ' -f1)" == "$want" ]] || fail "the published laavu-linux-$arch does not match its manifest"
chmod +x "$work/laavu"
check_flags "$work/laavu" "the published laavu $(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$work/manifest.json")"
