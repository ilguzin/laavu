# Сквозные тесты клиента

`make e2e` из корня: собирает клиент, `laavud`, заглушку `test/fakeclaude` и гоняет Playwright против двух `laavud dev`: на `:7700` без входа и на `:7800` с passkey (ссылка регистрации берётся из `.server-7800.log`). Каждый сервер получает свой дом агента и свой каталог сокетов tmux в `.state-<порт>/`.

Браузер: `npx playwright install chromium`. На хосте без sudo недостающие системные библиотеки Chromium (libnss3, libnspr4, libatk и др.) распаковываются из `.deb` без установки:

```sh
mkdir -p ~/.local/opt/pw-libs/debs && cd ~/.local/opt/pw-libs/debs
apt-get download libnspr4 libnss3 libatk1.0-0t64 libatk-bridge2.0-0t64 libxcomposite1 libxdamage1 \
  libxfixes3 libxrandr2 libasound2t64 libatspi2.0-0t64 libxrender1 libxi6 libxres1
for d in *.deb; do dpkg-deb -x "$d" ..; done
```

Конфиг подхватывает `~/.local/opt/pw-libs` сам.
