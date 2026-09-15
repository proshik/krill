.PHONY: generate build build-cli run test test-integration acceptance-selfupdate acceptance-vm-smoke lint-migrations tidy db-up db-down css css-watch

TAILWIND = ./tools/tailwindcss
SQUAWK = ./tools/squawk

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

# Live self-update acceptance in a disposable Lima VM (test/acceptance, see
# docs/development.md). Not part of `make test`. acceptance-selfupdate
# publishes throwaway releases to KRILL_ACCEPT_REPO; acceptance-vm-smoke
# touches no GitHub repository.
acceptance-selfupdate:
	KRILL_ACCEPT=1 go test -tags=acceptance -timeout 150m -v -run TestSelfUpdateAcceptance ./test/acceptance/

acceptance-vm-smoke:
	KRILL_ACCEPT=1 go test -tags=acceptance -timeout 40m -v -run TestVMSmoke ./test/acceptance/

# Migrations numbered at or below this predate the compatibility policy (a migration must keep
# the previous release bootable, so self-update can roll back); only later ones are linted.
MIGRATION_COMPAT_FROM ?= 46
SQUAWK_CONFIG = .squawk.toml
SQUAWK_FIXTURES = internal/database/testdata/squawk

# Lints new migrations with squawk (rules in .squawk.toml), then checks the rule set still has
# teeth: breaking.sql must report every `-- expect: <rule>` it lists and compatible.sql must pass.
lint-migrations:
	@test -x $(SQUAWK) || { echo "lint-migrations: $(SQUAWK) not found; run ./tools/get-squawk.sh" >&2; exit 1; }
	@files=$$(printf '%s\n' internal/database/migrations/*.up.sql | \
		awk -v from=$(MIGRATION_COMPAT_FROM) '{ n = $$0; sub(/.*\//, "", n); sub(/_.*/, "", n); if (n + 0 > from) print }'); \
	if [ -z "$$files" ]; then \
		echo "lint-migrations: no migrations above $(MIGRATION_COMPAT_FROM), nothing to lint"; \
	else \
		$(SQUAWK) --config $(SQUAWK_CONFIG) $$files || exit 1; \
	fi
	@if out=$$($(SQUAWK) --config $(SQUAWK_CONFIG) --reporter gcc $(SQUAWK_FIXTURES)/breaking.sql 2>&1); then \
		echo "lint-migrations: self-check failed: squawk accepted $(SQUAWK_FIXTURES)/breaking.sql" >&2; exit 1; \
	fi; \
	rules=$$(sed -n 's/^-- expect: *//p' $(SQUAWK_FIXTURES)/breaking.sql); \
	[ -n "$$rules" ] || { echo "lint-migrations: self-check failed: no '-- expect:' lines in breaking.sql" >&2; exit 1; }; \
	for rule in $$rules; do \
		printf '%s\n' "$$out" | grep -q "warning: $$rule " || { \
			echo "lint-migrations: self-check failed: $$rule is no longer reported for breaking.sql" >&2; \
			printf '%s\n' "$$out" >&2; exit 1; }; \
	done
	@$(SQUAWK) --config $(SQUAWK_CONFIG) $(SQUAWK_FIXTURES)/compatible.sql >/dev/null || { \
		echo "lint-migrations: self-check failed: squawk rejected $(SQUAWK_FIXTURES)/compatible.sql" >&2; \
		$(SQUAWK) --config $(SQUAWK_CONFIG) $(SQUAWK_FIXTURES)/compatible.sql >&2; exit 1; }
	@echo "lint-migrations: ok (self-check passed)"

tidy:
	go mod tidy

db-up:
	docker compose -f docker-compose.dev.yml up -d

db-down:
	docker compose -f docker-compose.dev.yml down
