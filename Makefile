.PHONY: generate build run test test-integration tidy db-up db-down

generate:
	go tool templ generate
	go tool sqlc generate

build: generate
	go build -o bin/krill ./cmd/krill

run: generate
	go run ./cmd/krill

test:
	go test ./...

test-integration:
	go test -tags=integration ./...

tidy:
	go mod tidy

db-up:
	docker compose -f docker-compose.dev.yml up -d

db-down:
	docker compose -f docker-compose.dev.yml down
