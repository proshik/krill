.PHONY: generate build run test test-integration tidy db-up db-down css css-watch

TAILWIND = ./tools/tailwindcss

generate:
	go tool templ generate
	go tool sqlc generate
	$(TAILWIND) -i internal/web/styles/input.css -o internal/web/static/app.css --minify

css:
	$(TAILWIND) -i internal/web/styles/input.css -o internal/web/static/app.css --minify

css-watch:
	$(TAILWIND) -i internal/web/styles/input.css -o internal/web/static/app.css --watch

build: generate
	go build -o bin/krill ./cmd/krill

run: generate
	go run ./cmd/krill

# testcontainers on Colima needs the host docker socket + the in-VM socket path for Ryuk
TC_ENV = DOCKER_HOST=unix://$(HOME)/.colima/default/docker.sock TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock

test:
	$(TC_ENV) go test ./...

test-integration:
	go test -tags=integration ./...

tidy:
	go mod tidy

db-up:
	docker compose -f docker-compose.dev.yml up -d

db-down:
	docker compose -f docker-compose.dev.yml down
