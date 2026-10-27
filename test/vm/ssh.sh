#!/usr/bin/env bash
# ssh into the test VM: ./ssh.sh [command]
source "$(dirname "$0")/lib.sh"
exec ssh "${SSH_OPTS[@]}" ubuntu@127.0.0.1 "$@"
