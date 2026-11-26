package volume

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ctxVolStore records whether SetVolumeBackupResult got a live context.
type ctxVolStore struct {
	mockVolStore
	ctxErr error
}

func (s *ctxVolStore) SetVolumeBackupResult(ctx context.Context, id int64, now time.Time, status, msg string) error {
	s.ctxErr = ctx.Err()
	return s.mockVolStore.SetVolumeBackupResult(ctx, id, now, status, msg)
}

// TestFailRecordsTheErrorPastTheDeadline: see the backup package's test of
// the same name.
func TestFailRecordsTheErrorPastTheDeadline(t *testing.T) {
	st := &ctxVolStore{}
	svc := &VolumeService{store: st, inFlight: map[int64]bool{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	_ = svc.fail(ctx, 1, time.Now(), errors.New("upload: context deadline exceeded"))
	if len(st.results) != 1 || st.results[0] != "error" || st.ctxErr != nil {
		t.Fatalf("failure written as %v on a context ending with %v", st.results, st.ctxErr)
	}
}
