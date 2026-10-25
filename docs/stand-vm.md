# Стенд: проверки на тестовой ВМ

Как tmux и меры изоляции ведут себя на Ubuntu 24.04: проверка перед началом разработки. ВМ: Ubuntu 24.04.5 LTS из облачного образа, 2 CPU, 2 ГБ, диск 20 ГБ, systemd 255.4-1ubuntu8.18, tmux 3.4. Проверено 2026-10-24.

## ВМ

Скрипты в `test/vm/`, параметры через переменные окружения (`VM_NAME`, `VM_CPUS`, `VM_RAM`, `VM_DISK_MB`, `VM_SSH_PORT`, `VM_PASSWORD`, `VM_FORWARD_PORTS`), общая часть — `lib.sh`.

- `./create.sh` — VDI из `~/vm-images/noble-server-cloudimg-amd64.vmdk`, расширение до 20 ГБ, seed ISO NoCloud, проброс `127.0.0.1:2222→22` и `8443`, `8444`, `8450–8459` один к одному, cloud-init, снапшот `clean`, старт. Повторный запуск только стартует ВМ.
- `./reset.sh` — откат к `clean` и старт с ожиданием ssh, около 30 с.
- `./ssh.sh [команда]` — `ubuntu@127.0.0.1:2222` по ключу `~/.ssh/id_ed25519`, без known_hosts.
- `./destroy.sh` — удаление ВМ с дисками.

У `ubuntu` sudo без пароля, ключ хоста и пароль `laavu-test`, вход по паролю по ssh включён — для теста `laavu install --ssh`. Лог консоли ВМ — `~/vm-images/laavu-test/console.log`.

Грабли: сеть контейнеров docker в bridge-режиме на хосте не работает (apk висит), ISO собирается с `--network host`. После снятия seed-диска cloud-init в снапшоте `clean` в состоянии `disabled` — это нормально, повторно он не нужен.

Эксперименты лежат рядом и повторяются: `test/vm/stand/tmux-hooks.sh` (от пользователя), `stand/iso-setup.sh` и `stand/iso-check.sh` (от root, между ними перезагрузка): `./ssh.sh 'sudo bash -s' < stand/iso-setup.sh`.

## tmux 3.4

Подтвердилось:

- Конфиг через `tmux -L laavu -f conf` принимается без ошибок: `status off`, `prefix None`, `default-terminal tmux-256color`, `set -ga terminal-features ",xterm-256color:RGB:usstyle:hyperlinks:sync:clipboard"`, `set-clipboard on`, `escape-time 10`, `extended-keys on` (серверная опция, `set -s`), `history-limit`, `aggressive-resize`, `window-size latest`, `remain-on-exit on`.
- Хуки `session-created`, `session-closed`, `session-renamed`, `pane-died` срабатывают и для сессий, созданных отдельным вызовом `tmux -L laavu new -d -s x` мимо воркера. `session-closed` срабатывает и при закрытии последней сессии, перед выходом сервера.
- Пользовательские опции `@laavu-*` переезжают вместе с сессией при `rename-session`.
- `list-sessions -F` отдаёт `#{pid}` и `#{start_time}` сервера, `#{session_path}` (папка из `-c`), `#{pane_dead}`, `#{pane_dead_status}`, `#{pane_start_command}`.

Какой формат брать в каком хуке (остальные дают чужую или пустую сессию):

| Хук | Имя сессии | Заметки |
|---|---|---|
| `session-created` | `#{hook_session_name}` (`#{session_name}` тоже верен) | |
| `session-renamed` | `#{hook_session_name}` — новое имя | старое имя не передаётся, нужна сверка снимка |
| `session-closed` | только `#{hook_session_name}` | `#{session_name}` здесь — другая, живая сессия |
| `pane-died` | `#{session_name}`, панель — `#{hook_pane}` | `#{hook_session_name}` пустой |

Подводные камни:

- **Внутри `run-shell` форматы надо экранировать.** `#{hook_session}` раскрывается в `$0`, и шелл превращает его в `sh`. Рабочий вид: `set-hook -g session-closed "run-shell -b 'laavud hook tmux session-closed #{q:hook_session_name}'"` — `#{q:…}` плюс одинарные кавычки.
- `pane_start_command` у сессии с шеллом по умолчанию пустой, у сессии с командой — строка в кавычках tmux (`"sleep 1; exit 3"`). Для снимка лучше хранить команду запуска в своей опции (`@laavu-cmd`), а `pane_start_command` брать только для сессий, заведённых мимо воркера.
- `show -g` принимает одну опцию за вызов.

## Изоляция

Пользователи `lv-a` (uid 1001) и `lv-b` (1002): `useradd -m -d /srv/laavu/agents/<имя> -s /bin/bash -G laavu-agents -c "umask=077"`, дом `0700`, `passwd -l`, `loginctl enable-linger`.

| Мера | Результат |
|---|---|
| `hidepid=invisible,gid=procview` в fstab (`proc /proc proc defaults,hidepid=invisible,gid=procview 0 0`) | работает после перезагрузки. logind, `loginctl`, linger, `user@.service`, `systemctl --user`, `systemd-run --user --scope` у агента работают **без** drop-in `SupplementaryGroups=procview` для logind и polkit; ошибок в журнале нет. `lv-a` процессов `lv-b` не видит, `ubuntu` в `procview` видит |
| Дом `0700` | `lv-a` не читает дом `lv-b` |
| umask | GECOS `umask=077` через pam_umask действует и в `sudo -u`, и в `user@.service` (umask `0077`): файлы `lv-b` в `/tmp` — `-rw-------`. Для системного юнита воркера (без PAM) всё равно нужен `UMask=0077` в юните |
| `MemoryMax`, `TasksMax` в `user-<uid>.slice.d/laavu.conf` | применяются (`memory.max`, `pids.max` слайса) |
| **`SocketBindAllow` / `SocketBindDeny`** | **не работают вообще.** systemd в Ubuntu 24.04 собран с `-BPF_FRAMEWORK`, настройка молча игнорируется и на слайсе, и даже на сервисе (`systemd-run -p SocketBindDeny=any` биндит порт). `systemctl show` показывает настройку, но BPF-программы на cgroup нет |
| `IPAddressDeny` | работает (встроенная BPF-программа, без BPF_FRAMEWORK) — на случай, если понадобится |
| nftables по `meta skuid` | работает: `lv-a` не подключается к серверу `lv-b` в его диапазоне ни по `127.0.0.1`, ни по `::1`, ни по внешнему адресу ВМ (тоже через `lo`); сам `lv-b`, root и `ubuntu` подключаются. Исходящие наружу (curl, DNS) не задеты, `bind()` на порт 0 работает |
| `sudo -n true` от агента | отказ |

Рабочее правило nftables (файл `/etc/laavu/nftables.nft`, повторно загружается без ошибок):

```
table inet laavu
delete table inet laavu
table inet laavu {
  chain output {
    type filter hook output priority 0; policy accept;
    oif "lo" tcp dport 20000-20999 meta skuid != 1001 meta skuid { 1002 } reject
    oif "lo" tcp dport 21000-21999 meta skuid != 1002 meta skuid { 1001 } reject
  }
}
```

Таблица сама не переживает перезагрузку. Пакетный `nftables.service` не годится: его `/etc/nftables.conf` начинается с `flush ruleset` и снёс бы правила UFW. Работает свой юнит, проверено вместе с включённым UFW после перезагрузки:

```
# /etc/systemd/system/laavu-nftables.service
[Unit]
Description=Laavu: правила nftables между агентами
Wants=network-pre.target
Before=network-pre.target
After=ufw.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/laavu/nftables.nft
ExecStop=/usr/sbin/nft delete table inet laavu
[Install]
WantedBy=multi-user.target
```

Владелец слушающего сокета виден в `/proc/net/tcp` и `/proc/net/tcp6` (8-й столбец, uid) любому пользователю, `hidepid` на это не влияет.

## Что поменять в архитектуре

1. **`SocketBindAllow` на Ubuntu 24.04 не годится.** Ограничить, какие порты агент слушает, средствами systemd нельзя. Чем заменить:
   - защита диапазона агента от чужих подключений — правило nftables выше, оно работает и само по себе закрывает «подключиться к сервисам другого агента» в его диапазоне;
   - чужой агент может занять порт из диапазона соседа раньше него. Тогда прокси `laavud` (поток 6) открыл бы пользователю сервер не того агента. Поэтому прокси перед подключением сверяет uid владельца слушающего сокета по `/proc/net/tcp{,6}` с uid агента, а при несовпадении отказывает;
   - порты вне всех диапазонов (например, эфемерные слушатели Playwright или Jupyter) другим агентам доступны. Закрыть их правилом «агент ходит на `lo` только в свой диапазон» нельзя без поломки инструментов, которые слушают на случайном порту. Остаётся как допустимый остаток, вместе с абстрактными unix-сокетами;
   - `check-isolation`: проба «занять порт вне своего диапазона» убирается или становится предупреждением; вместо неё проверяется, что `lv-a` не подключается к слушателю `lv-b` в диапазоне `lv-b`.
   - в таблицу мер `server.md` — строку про `SocketBindAllow` заменить на nftables + проверку владельца в прокси. Если позже понадобится именно запрет `bind`, путь — своя BPF-программа `cgroup/bind4|bind6` или systemd с BPF_FRAMEWORK (проверить в Ubuntu 26.04).
2. **nftables — свой юнит `laavu-nftables.service`**, а не `nftables.service` и не `/etc/nftables.conf`.
3. **Drop-in для logind и polkit под `hidepid` не нужен** — из установщика убрать, если планировался.
4. **Хуки tmux** — форматы по таблице выше и обязательно `#{q:…}` в одинарных кавычках. `session-renamed` старого имени не даёт, `pane-died` даёт имя через `#{session_name}`, а `session-closed` — только через `#{hook_session_name}`. Подход «на любой хук — сверка снимка со списком сессий» подтверждается как правильный.
5. **umask:** pam_umask с `umask=077` в GECOS работает для всего, что идёт через PAM, включая пользовательский экземпляр systemd; `UMask=0077` нужен только в системном юните воркера.
