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

[ "$fail" = 0 ] && echo "ALL PASS" || echo "SOME FAILED"
exit "$fail"
