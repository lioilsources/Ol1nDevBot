VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/lioilsources/ol1ndevbot

.PHONY: build test vet generate golden run docker

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/ol1nbot ./cmd/ol1nbot

test:
	go test ./...

vet:
	go vet ./...

# Regenerate sqlc code after editing internal/store/query.sql or migrations.
generate:
	sqlc generate

# Rewrite golden embed files after an intentional formatter change.
golden:
	go test ./internal/discord -run TestRenderGolden -update

# Local run against the dev guild: needs .env and apps.yaml.
run: build
	set -a && . ./.env && set +a && DB_PATH=./ol1nbot.db APPS_YAML=./apps.yaml ./bin/ol1nbot

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .
