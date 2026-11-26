package backup

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ctxStore records whether SetBackupResult got a live context.
type ctxStore struct {
	migratingStore
	status string
	ctxErr error
}

func (s *ctxStore) SetBackupResult(ctx context.Context, _ int64, _ time.Time, status, _ string) error {
	s.status, s.ctxErr = status, ctx.Err()
	return ctx.Err()
}

// TestFailRecordsTheErrorPastTheDeadline: a run that failed because its
// deadline passed must still write its error, or the previous "ok" stays and
// reads as this run's result.
func TestFailRecordsTheErrorPastTheDeadline(t *testing.T) {
	st := &ctxStore{}
	svc := New(&noopExecer{}, st, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	_ = svc.fail(ctx, 1, time.Now(), errors.New("upload: context deadline exceeded"))
	if st.status != "error" || st.ctxErr != nil {
		t.Fatalf("failure written with status %q on a context ending with %v", st.status, st.ctxErr)
	}
}
