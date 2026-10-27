#!/usr/bin/env bash
# Step 0.4: tmux hooks and options on the VM. Run on the VM as a regular user.
set -u
command -v tmux >/dev/null || sudo apt-get install -y -q tmux >/dev/null
echo "tmux: $(tmux -V)"
d=$(mktemp -d); log=$d/hooks.log; conf=$d/tmux.conf
fmt='name=#{session_name} hsn=#{hook_session_name} hs=#{hook_session} hp=#{hook_pane} pane=#{pane_id} path=#{session_path} pid=#{pid}'
cat >"$conf" <<CONF
set -g status off
set -g prefix None
set -g default-terminal tmux-256color
set -ga terminal-features ",xterm-256color:RGB:usstyle:hyperlinks:sync:clipboard"
set -g set-clipboard on
set -s escape-time 10
set -s extended-keys on
set -g history-limit 50000
setw -g aggressive-resize on
set -g window-size latest
set -g remain-on-exit on
set -g monitor-activity on
set -g monitor-silence 0
set-hook -g session-created 'run-shell -b "echo created $fmt >> $log"'
set-hook -g session-closed 'run-shell -b "echo closed $fmt >> $log"'
set-hook -g session-renamed 'run-shell -b "echo renamed $fmt >> $log"'
set-hook -g pane-died 'run-shell -b "echo died $fmt >> $log"'
CONF
T="tmux -L laavutest -f $conf"
echo "--- config: errors at startup"
$T new -d -s keeper -c /tmp 2>&1 && echo "started without errors"
$T new -d -s first -c /tmp
$T show -g remain-on-exit prefix window-size 2>&1
$T show -s extended-keys escape-time 2>&1
mkdir -p $d/work
$T new -d -s second -c $d/work 'sleep 1; exit 3'
$T set -t first @laavu-status working
$T set -t first @laavu-claude 1234-uuid
$T rename-session -t first renamed1
echo "--- options after rename: $($T show -t renamed1 -v @laavu-status) $($T show -t renamed1 -v @laavu-claude)"
sleep 2
echo "--- list-sessions"
$T ls -F '#{session_name}|#{session_path}|#{pid}|#{start_time}|#{@laavu-status}|#{pane_dead}|#{pane_dead_status}|#{pane_start_command}|#{pane_current_command}'
$T kill-session -t second
sleep 1
$T kill-session -t renamed1
sleep 1
$T kill-session -t keeper
sleep 1
echo "--- hook log"
cat $log
$T kill-server 2>/dev/null
rm -rf $d
