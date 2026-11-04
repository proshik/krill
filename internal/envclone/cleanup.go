package envclone

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Cleanup must outlive request cancellation without outliving shutdown forever.
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// Only a missing row is a tenant-safe 404. Infrastructure failures retain a
// separate safe category; SQL-bearing errors never become UI text or logs.
func metadataError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return ErrFailed
}
