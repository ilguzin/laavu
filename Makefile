# Сборка Laavu. Go — в ~/.local/opt/go/bin или в PATH.
export PATH := $(HOME)/.local/opt/go/bin:$(PATH)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
CMDS := $(notdir $(wildcard cmd/*))

.PHONY: build web web-copy test e2e clean

# Бинарники в bin/: клиент вшивается в laavud из internal/webui/dist.
build:
	@mkdir -p bin
	@for c in $(CMDS); do echo "go build cmd/$$c"; CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$$c ./cmd/$$c || exit 1; done

# Клиент: web/dist → internal/webui/dist.
web:
	cd web && npm ci --no-audit --no-fund && npm run build
	$(MAKE) web-copy

web-copy:
	find internal/webui/dist -mindepth 1 ! -name .keep -delete
	cp -r web/dist/. internal/webui/dist/

test:
	go vet ./...
	go test ./...

# Сквозные тесты в браузере против laavud dev.
e2e: web build
	go build -o bin/fakeclaude ./test/fakeclaude
	cd test/e2e && npm ci --no-audit --no-fund && npx playwright test

clean:
	rm -rf bin web/dist
	find internal/webui/dist -mindepth 1 ! -name .keep -delete

# Тестовая ВМ в VirtualBox (см. docs/dev.md).
.PHONY: vm-create vm-reset vm-e2e
vm-create:
	test/vm/create.sh
vm-reset:
	test/vm/reset.sh
vm-e2e: web build
	test/vm/e2e-server.sh
	test/vm/e2e-ssh.sh
