# Claude Code on the test stand

Before writing any code we checked how Claude Code behaves in tmux and with hooks. Claude Code 2.1.310, tmux 3.6, developer host, 2026-10-24. Hooks are in `.claude/settings.json` of a temporary folder (project level). A logger script records the event, stdin, and `TMUX`, `TMUX_PANE`.

One limitation. The nested `claude` on the host is not logged in ("Not logged in · Please run /login"), because the host user's login doesn't carry over into it. So nothing that needs a model response got verified: `Stop`, `decision: block`, `stop_hook_active`, `PreToolUse`/`PostToolUse`, `Notification`, `/compact`, token counters on a fresh response. That part is left for the VM check with an agent token. Everything that works without the model was verified.

## Confirmed

1. Hooks see tmux. For any `claude` run in a pane, the hook gets `TMUX=/tmp/tmux-1002/ccprobe,<pid>,<n>` and `TMUX_PANE=%0`, and `tmux display -p -t $TMUX_PANE '#S'` from the hook gives the session name (`probe1`, `probe2`…). The tmux socket is taken from `TMUX`, so `-L` is not needed in the hook.
2. `--settings` is merged with the other settings. A `UserPromptSubmit` hook from `--settings extra.json` and the same hook from the project settings both fired, in the same millisecond. We didn't check the user level (`~/.claude/settings.json`) so as not to touch the developer's settings. We assume the project level behaves the same. To be checked on the VM for an agent.
3. `SessionStart` fields:
   ```json
   {"session_id":"0d9cbbb7-…","transcript_path":"/home/dev/.claude/projects/-tmp-…-cc-probe/0d9cbbb7-….jsonl",
    "cwd":"/tmp/…/cc-probe","hook_event_name":"SessionStart","source":"startup","model":"claude-haiku-5-5"}
   ```
   `model` is present only on `startup`; `clear` and `resume` do not have it.
4. stdout of a `SessionStart` hook goes into the model context. A line from stdout ended up in the transcript as an `attachment` of type `hook_success` and in the system prompt as `SessionStart:startup hook success: …`. So the status hook must print nothing to stdout, on success or on error.
5. `/clear` gives a new identifier. Order: `SessionEnd` with the old `session_id` and `"reason":"clear"`, then `SessionStart` with a new `session_id`, `"source":"clear"` and a new `transcript_path`. The old conversation file remains.
6. `--resume <uuid>` keeps the identifier. `SessionStart` with the same `session_id`, `"source":"resume"`, the old conversation on screen. From another folder `--resume` also brought the conversation back (the transcript lives in the project folder of the original `cwd`), but then the session `cwd` is different. So we resume from the session folder anyway, as planned.
7. `SessionEnd` on `tmux kill-session` has `"reason":"other"`, and the hook can no longer find the session name (`display` gives nothing, the session is gone). The daemon has to learn about closing from the tmux hook instead of `SessionEnd`.
8. Pasting through the tmux buffer works. `load-buffer` of three lines + `paste-buffer -p -d` + an immediate `send-keys Enter` with no pause went as one message, the transcript has `content` with `\n` inside, `promptSource: "typed"`. Only one `UserPromptSubmit` arrived. No pause is needed (on other versions keep 50–100 ms as a margin).
9. Without a model response `Stop` never arrives. After the "Not logged in" error there was only `UserPromptSubmit` and nothing else. So an API error does not produce the "waiting" status: after `UserPromptSubmit` a timeout or a transcript check is needed (`"isApiErrorMessage": true` on the `assistant` record).
10. Token counters in the transcript are on `"type":"assistant"` records, field `message.usage`: `input_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`, `output_tokens`. Context fill of the last response = `input_tokens + cache_creation_input_tokens + cache_read_input_tokens` (+ `output_tokens` if counting what will go into the next request). Example from a real transcript: `2 + 3042 + 67425`. Take the last record with `isSidechain: false` (subagents have their own). The window size is not in the transcript. `model` is (`claude-opus-5-5`), so the window size has to come from a model table.
11. The `statusLine` input knows the window size. The command from `statusLine` in the settings receives JSON with `context_window.context_window_size` (`1000000` for Haiku 5.5), `used_percentage`, `remaining_percentage`, `current_usage`. That gives the fill percentage without a table of our own. But there is only one `statusLine` per settings file and it prints a line in the TUI, so if the agent or the user sets up their own line, ours is gone. One way around it: our `statusLine` writes the percentage to a file and prints an empty line.
12. The folder trust dialog appears on the first run in a new folder ("No, exit" is selected by default), even with `--resume` from a new folder. Acceptance is written to `~/.claude.json` → `projects["<path>"].hasTrustDialogAccepted: true`. For agents, the worker or the template has to mark folders as trusted in advance. Otherwise a new session gets stuck on this dialog. If `.claude/settings.json` has `permissions.allow`, the dialog lists them.
13. The permission mode of the nested `claude` inherited "auto mode" from the developer's user settings. So an agent's mode has to be set in its `~/.claude/settings.json`.

## Not verified (needs a logged-in claude)

- `Stop` with `{"decision":"block","reason":…}` continues the turn, and the second `Stop` has `stop_hook_active: true`.
- `PreToolUse`/`PostToolUse`: fields `tool_name`, `tool_input` (file path for Edit/Write).
- `Notification`: when it arrives on a permission request and with which fields (`message`, `notification_type`?).
- `/compact` → `SessionStart` with `"source":"compact"` and the same or a new `session_id`.
- Hooks from the agent's user-level `~/.claude/settings.json`.
- Running under a different `HOME` (the agent home). To be checked on the VM.

