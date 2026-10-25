#!/usr/bin/env bash
# ssh на тестовую ВМ: ./ssh.sh [команда]
source "$(dirname "$0")/lib.sh"
exec ssh "${SSH_OPTS[@]}" ubuntu@127.0.0.1 "$@"
