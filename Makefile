.PHONY: generate build build-cli run test test-integration tidy db-up db-down css css-watch

TAILWIND = ./tools/tailwindcss

# Shared by both binaries: internal/buildinfo.Version, stamped via ldflags so
# `krill --version` and `krill-cli version` report the same thing a release
# tag would.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VERSION_LDFLAGS = -X github.com/proshik/krill/internal/buildinfo.Version=$(VERSION)

generate:
	go tool templ generate
	go tool sqlc generate
	$(TAILWIND) -i internal/web/styles/input.css -o internal/web/static/app.css --minify

css:
	$(TAILWIND) -i internal/web/styles/input.css -o internal/web/static/app.css --minify

css-watch:
	$(TAILWIND) -i internal/web/styles/input.css -o internal/web/static/app.css --watch

build: generate
	go build -ldflags "$(VERSION_LDFLAGS)" -o bin/krill ./cmd/krill

# The CLI deliberately does NOT depend on `generate`: it imports none of the
# templ, sqlc or Tailwind output, and requiring ./tools/tailwindcss to exist
# just to build a client binary would be a pointless prerequisite.
build-cli:
	go build -trimpath -ldflags "-s -w $(VERSION_LDFLAGS)" -o bin/krill-cli ./cmd/krill-cli

run: generate
	go run ./cmd/krill

# testcontainers on Colima needs the host docker socket + the in-VM socket path for Ryuk
TC_ENV = DOCKER_HOST=unix://$(HOME)/.colima/default/docker.sock TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock

test:
	$(TC_ENV) go test ./...

test-integration:
	$(TC_ENV) go test -tags=integration ./...

tidy:
	go mod tidy

db-up:
	docker compose -f docker-compose.dev.yml up -d

db-down:
	docker compose -f docker-compose.dev.yml down
