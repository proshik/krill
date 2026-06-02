package org

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMapUniqueErr(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505"}
	if !errors.Is(mapUniqueErr(fmt.Errorf("wrap: %w", pgErr)), ErrSlugTaken) {
		t.Error("23505 wrapped error must map to ErrSlugTaken")
	}
	if mapUniqueErr(nil) != nil {
		t.Error("nil must stay nil")
	}
	other := errors.New("some other error")
	if !errors.Is(mapUniqueErr(other), other) {
		t.Error("non-unique error must pass through unchanged")
	}
	fake := errors.New("log line mentioning 23505 but not a real pg error")
	if errors.Is(mapUniqueErr(fake), ErrSlugTaken) {
		t.Error("must NOT treat arbitrary text containing 23505 as unique violation")
	}
}
