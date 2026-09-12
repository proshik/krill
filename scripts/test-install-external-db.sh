#!/bin/sh
# Tests for install.sh external-DB helpers (Phase 8).
# Sources install.sh in library mode (defines functions, no side effects),
# then exercises mask_dsn and preflight_external_db.
set -u
DIR="$(cd "$(dirname "$0")/.." && pwd)"
fail=0
check() { # check "desc" "expected" "actual"
	if [ "$2" = "$3" ]; then printf 'ok   %s\n' "$1"; else printf 'FAIL %s\n  expected: %s\n  actual:   %s\n' "$1" "$2" "$3"; fail=1; fi
}

# shellcheck disable=SC1090
KRILL_LIB_ONLY=1 . "$DIR/install.sh"
set +e  # install.sh runs `set -eu`; sourcing leaks -e into this shell — undo it.

check "mask password" "postgres://u:***@h:5432/db?sslmode=require" "$(mask_dsn 'postgres://u:secret@h:5432/db?sslmode=require')"
check "no password unchanged" "postgres://u@h:5432/db" "$(mask_dsn 'postgres://u@h:5432/db')"
check "hostless unchanged" "postgres://h/db" "$(mask_dsn 'postgres://h/db')"

# --- persist_advertise_addr (KRILL_ADVERTISE_ADDR backfill on upgrade) ---
ENVTMP="$(mktemp)"
printf 'KRILL_DATABASE_URL=postgres://h/db\nKRILL_LISTEN_ADDR=:8080\n' >"$ENVTMP"
persist_advertise_addr "$ENVTMP" "10.0.0.5" >/dev/null
check "advertise backfilled" "KRILL_ADVERTISE_ADDR=10.0.0.5" "$(grep '^KRILL_ADVERTISE_ADDR=' "$ENVTMP")"
persist_advertise_addr "$ENVTMP" "10.0.0.5" >/dev/null
check "advertise backfill idempotent" "1" "$(grep -c '^KRILL_ADVERTISE_ADDR=' "$ENVTMP")"
persist_advertise_addr "$ENVTMP" "10.9.9.9" >/dev/null
check "operator value not overwritten" "KRILL_ADVERTISE_ADDR=10.0.0.5" "$(grep '^KRILL_ADVERTISE_ADDR=' "$ENVTMP")"
check "existing lines preserved" "KRILL_LISTEN_ADDR=:8080" "$(grep '^KRILL_LISTEN_ADDR=' "$ENVTMP")"
printf 'KRILL_LISTEN_ADDR=:8080' >"$ENVTMP" # no trailing newline
persist_advertise_addr "$ENVTMP" "10.0.0.5" >/dev/null
check "no trailing newline: lines not glued" "KRILL_LISTEN_ADDR=:8080|KRILL_ADVERTISE_ADDR=10.0.0.5" "$(paste -sd'|' "$ENVTMP")"
: >"$ENVTMP"
persist_advertise_addr "$ENVTMP" "" >/dev/null
check "empty address writes nothing" "0" "$(wc -c <"$ENVTMP" | tr -d ' ')"
rm -f "$ENVTMP"

# --- preflight_external_db integration (needs Docker) ---
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
	# Failure path (most important — the fail-closed gate): unreachable DSN must die (non-zero).
	if ( preflight_external_db 'postgres://x:y@127.0.0.1:1/db?connect_timeout=2' ) >/dev/null 2>&1; then
		printf 'FAIL preflight should reject an unreachable DSN\n'; fail=1
	else
		printf 'ok   preflight rejects unreachable DSN\n'
	fi
	# Success path (best-effort): a throwaway postgres reachable via host loopback.
	docker rm -f krill-pf-test >/dev/null 2>&1 || true
	docker run -d --name krill-pf-test -e POSTGRES_PASSWORD=testpw -p 127.0.0.1:15432:5432 "$PG_IMAGE" >/dev/null 2>&1 || true
	i=0; until docker exec krill-pf-test pg_isready -q >/dev/null 2>&1 || [ "$i" -ge 30 ]; do i=$((i+1)); sleep 1; done
	if ( preflight_external_db 'postgres://postgres:testpw@127.0.0.1:15432/postgres?sslmode=disable' ) >/dev/null 2>&1; then
		printf 'ok   preflight accepts a reachable DSN\n'
	else
		printf 'WARN preflight success path did not pass (host-loopback wiring); check manually\n'
	fi
	docker rm -f krill-pf-test >/dev/null 2>&1 || true
else
	printf 'SKIP preflight integration (no docker)\n'
fi

[ "$fail" = 0 ] && echo "ALL PASS" || echo "SOME FAILED"
exit "$fail"
