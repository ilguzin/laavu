# Стенд: Claude Code под модель сессий

Как Claude Code ведёт себя в tmux и с хуками: проверка перед началом разработки. Claude Code 2.1.310, tmux 3.6, хост разработчика, 2026-10-24. Хуки — в `.claude/settings.json` временной папки (проектный уровень), скрипт-логгер пишет событие, stdin и `TMUX`, `TMUX_PANE`.

**Ограничение стенда.** Вложенный `claude` на хосте не залогинен («Not logged in · Please run /login»): авторизация пользователя хоста в него не попадает. Поэтому всё, что требует ответа модели, не проверено: `Stop`, `decision: block`, `stop_hook_active`, `PreToolUse`/`PostToolUse`, `Notification`, `/compact`, счётчики токенов у свежего ответа. Это переносится в проверку на ВМ с токеном агента. Проверено всё, что работает без модели.

## Подтверждено

1. **Хуки видят tmux.** У любого запуска `claude` в панели хук получает `TMUX=/tmp/tmux-1002/ccprobe,<pid>,<n>` и `TMUX_PANE=%0`; `tmux display -p -t $TMUX_PANE '#S'` из хука даёт имя сессии (`probe1`, `probe2`…). Сокет tmux берётся из `TMUX`, так что `-L` в хуке указывать не нужно.
2. **`--settings` сливается, а не заменяет.** Хук `UserPromptSubmit` из `--settings extra.json` и тот же хук из настроек проекта сработали оба, в одну миллисекунду. Пользовательский уровень (`~/.claude/settings.json`) не проверялся, чтобы не трогать настройки разработчика; проектный уровень считаем эквивалентом — проверить на ВМ у агента.
3. **Поля `SessionStart`:**
   ```json
   {"session_id":"0d9cbbb7-…","transcript_path":"/home/dev/.claude/projects/-tmp-…-cc-probe/0d9cbbb7-….jsonl",
    "cwd":"/tmp/…/cc-probe","hook_event_name":"SessionStart","source":"startup","model":"claude-haiku-5-5"}
   ```
   `model` есть только при `startup`, у `clear` и `resume` его нет.
4. **stdout хука `SessionStart` уходит в контекст модели.** Строка из stdout попала в транскрипт как `attachment` типа `hook_success` и в системный промпт как `SessionStart:startup hook success: …`. Хук статуса обязан молчать: ничего не печатать в stdout ни при успехе, ни при ошибке.
5. **`/clear` даёт новый идентификатор.** Порядок: `SessionEnd` со старым `session_id` и `"reason":"clear"`, затем `SessionStart` с новым `session_id`, `"source":"clear"` и новым `transcript_path`. Файл старого разговора остаётся.
6. **`--resume <uuid>` сохраняет идентификатор.** `SessionStart` с тем же `session_id`, `"source":"resume"`, старый разговор на экране. Из другой папки `--resume` тоже поднял разговор (транскрипт лежит в папке проекта исходного `cwd`), но `cwd` сессии тогда другой — поднимать всё равно из папки сессии, как задумано.
7. **`SessionEnd` при `tmux kill-session`:** `"reason":"other"`, а имя сессии из хука уже не узнать (`display` даёт пусто, сессии нет). Демону закрытие надо брать из хука tmux, а не из `SessionEnd`.
8. **Вставка через буфер tmux.** `load-buffer` трёх строк + `paste-buffer -p -d` + сразу `send-keys Enter` без паузы: ушло одним сообщением, в транскрипте `content` с `\n` внутри, `promptSource: "typed"`. `UserPromptSubmit` пришёл один. Пауза не нужна (на других версиях оставить 50–100 мс как запас).
9. **Без ответа модели `Stop` не приходит.** После ошибки «Not logged in» были только `UserPromptSubmit` и больше ничего. Значит, ошибка API не даёт статуса «ждёт ответа» — после `UserPromptSubmit` нужен таймаут или проверка транскрипта (`"isApiErrorMessage": true` у записи `assistant`).
10. **Счётчики токенов в транскрипте** — у записей `"type":"assistant"`, поле `message.usage`: `input_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`, `output_tokens`. Заполненность контекста последнего ответа = `input_tokens + cache_creation_input_tokens + cache_read_input_tokens` (+ `output_tokens`, если считать то, что войдёт в следующий запрос). Пример из реального транскрипта: `2 + 3042 + 67425`. Брать последнюю запись с `isSidechain: false` (у субагентов свои). **Размера окна в транскрипте нет**: `model` есть (`claude-opus-5-5`), окно надо знать по таблице моделей.
11. **Размер окна знает вход `statusLine`.** Команда из `statusLine` в настройках получает JSON с `context_window.context_window_size` (у Haiku 5.5 — `1000000`), `used_percentage`, `remaining_percentage`, `current_usage`. Это готовый процент заполненности без своей таблицы, но `statusLine` один на настройки и выводит строку в TUI — если агент или пользователь заведёт свою строку, наша пропадёт. Вариант: наш `statusLine` пишет процент в файл и печатает пустую строку.
12. **Диалог доверия папке** появляется при первом запуске в новой папке (по умолчанию выбрано «No, exit»), даже при `--resume` из новой папки. Принятие пишется в `~/.claude.json` → `projects["<путь>"].hasTrustDialogAccepted: true`. Для агента: воркер или шаблон должны отмечать рабочие папки доверенными заранее, иначе новая сессия встанет на этом диалоге. Если в `.claude/settings.json` есть `permissions.allow`, диалог их перечисляет.
13. **Режим прав** у вложенного `claude` унаследовал «auto mode» из пользовательских настроек разработчика — у агента режим задаётся его `~/.claude/settings.json`.

## Не проверено (нужен залогиненный claude)

- `Stop` с `{"decision":"block","reason":…}` продолжает ход; `stop_hook_active: true` во втором `Stop`.
- `PreToolUse`/`PostToolUse`: поля `tool_name`, `tool_input` (путь файла у Edit/Write).
- `Notification`: когда приходит при запросе разрешения и с какими полями (`message`, `notification_type`?).
- `/compact` → `SessionStart` с `"source":"compact"` и тем же или новым `session_id`.
- Хуки из пользовательского уровня `~/.claude/settings.json` агента.
- Запуск под другим `HOME` (дом агента): не проверялось, проверить на ВМ.

