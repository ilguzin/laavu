# Разработка

Как собрать Laavu, прогнать тесты и проверить установку на тестовой ВМ. Устройство кода — в [`architecture.md`](architecture.md).

## Что нужно на машине разработчика

- Go 1.26+ (если Go стоит в `~/.local/opt/go`, `Makefile` добавляет его в `PATH` сам).
- Node 22+ для клиента и Playwright.
- tmux и git — для тестов воркера и демона.
- VirtualBox и docker — для тестовой ВМ (docker нужен только чтобы собрать ISO для cloud-init).
- Chromium для Playwright: `cd test/e2e && npx playwright install chromium`. Если в системе не хватает библиотек, а sudo нет, порядок обхода — в [`test/e2e/README.md`](../test/e2e/README.md).

## Сборка

```sh
make web     # клиент: web/ → web/dist → internal/webui/dist
make build   # bin/laavu и bin/laavud, клиент вшит в laavud
```

Без `make web` демон собирается, но вместо клиента отвечает «клиент не собран».

## Тесты

```sh
make test    # go vet и go test ./... — воркер и демон на настоящем tmux, с заглушкой claude
make e2e     # Playwright против laavud dev: сессии, файлы, просмотр, passkey, телефон
```

Заглушка `test/fakeclaude` вызывает хуки из `~/.claude/settings.json` и `--settings` с теми же полями, что настоящий Claude Code (сверено на стенде, [`stand-claude-code.md`](stand-claude-code.md)), пишет транскрипт со счётчиками и понимает `/clear`, `/compact`, `/exit`. Сообщение `write:путь` пишет файл через «инструмент Write».

## Клиент

`web/src`: Preact без библиотеки состояния (`store.ts`), тема — токены на `:root[data-theme]` в `style.css`, `theme.ts` выбирает тему и строит из тех же токенов тему xterm.

- Раскладка: рейка агентов (`Rail`), список сессий (`Sidebar`), группы вкладок (`Workspace`), навигатор по файлам (`Navigator`). На телефоне — по одному экрану и нижнее меню.
- Вкладка — сессия (`SessionView`: шапка, xterm, на телефоне полоса клавиш `KeyBar`) или файл (`FileView`: просмотр, исходник, правка, изменения). Группы вкладок 1–4 хранятся в `localStorage`.
- Отдельного поля ввода нет: ввод идёт прямо в терминал, путь после загрузки и цитата из файла вставляются в терминал без Enter.
- Редактор — CodeMirror 6 (`editor/cm.ts`), отдельный чанк. Сохранение — `PUT …/files/{папка}/file` с версией из чтения; другая версия на диске — `409`, клиент показывает сравнение.

## Режим разработки

```sh
bin/laavud dev --home /tmp/laavu-home --claude "$(pwd)/bin/fakeclaude"   # без входа, http://localhost:7700
bin/laavud dev --home /tmp/laavu-home --auth                             # с passkey, ссылка регистрации в выводе
```

Демон и воркер в одном процессе от текущего пользователя, origin просмотра — на `:7701`, dev-серверы — на `:7710–7719`. Дом агента — отдельная папка, не свой настоящий дом.

## Тестовая ВМ

```sh
test/vm/create.sh        # ВМ из облачного образа Ubuntu 24.04, снапшот clean
test/vm/reset.sh         # откат к clean (около 30 с)
test/vm/ssh.sh           # зайти на ВМ
test/vm/e2e-server.sh    # установка, два агента, изоляция, сессии, перезагрузка, браузер через Caddy
test/vm/e2e-ssh.sh       # laavu install --ssh по паролю, подписанные релизы, обновление и откат
```

Порты ВМ проброшены на хост один к одному: приложение `https://localhost:8443`, origin просмотра `:8444`, dev-серверы `:8450–8459`, ssh `:2222`. Сертификат выдаёт внутренний CA Caddy; браузер на хосте ругается на него, пока корень CA (`/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt` на ВМ) не добавлен в доверенные. Passkey с RP ID `localhost` работают.

**Доступ из tailnet.** `test/vm/tailnet.sh` пробрасывает те же порты на адрес хоста в tailnet и переводит Laavu на ВМ на имя хоста (`laavu install --host ХОСТ.<tailnet>.ts.net`). На своей машине нужно один раз добавить в доверенные корень CA Caddy (скрипт кладёт его в `~/laavu-test-root.crt` на хосте): без доверенного сертификата браузер отключает passkey. После `reset.sh` скрипт запускается заново.

Руками на ВМ:

```sh
test/vm/reset.sh
scp -P 2222 bin/laavu bin/laavud ubuntu@127.0.0.1:/tmp/
test/vm/ssh.sh 'sudo /tmp/laavu install --agent demo --install-claude'
```

или с хоста одной командой, как это будет делать владелец VPS:

```sh
bin/laavu install --ssh ubuntu@127.0.0.1:2222 --agent demo --install-claude
```

Без `--release` ставится локальная сборка без подписи — утилита предупреждает об этом.

## Релизы

Ключ подписи хранится у владельца проекта, не в CI.

```sh
bin/laavu release keygen --out ~/.config/laavu/release.key   # один раз; открытый ключ — в переменную LAAVU_PROJECT_KEY репозитория
git tag v0.1.0 && git push --tags                              # CI собирает черновик релиза с неподписанным manifest.json
# скачать manifest.json из черновика в каталог dist/, затем:
bin/laavu release sign --dir dist --key ~/.config/laavu/release.key
# загрузить dist/manifest.json.sig в релиз и опубликовать его
```

Сервер проверяет подпись ключом, закреплённым при установке (`/etc/laavu/release.pub`), и откатывается, если после обновления `laavud` не отвечает.

## Служебное

- `laavud call АГЕНТ ОПЕРАЦИЯ [JSON]` — запрос к воркеру агента напрямую (от `laavud` или root), для диагностики: `sudo runuser -u laavud -- laavud call demo sessions`.
- `laavu diag` — службы, журналы, правила, сокеты; без содержимого рабочих папок.
- `laavu check --json` — отчёт о состоянии, схема пойдёт в контракт с консолью (К1).
