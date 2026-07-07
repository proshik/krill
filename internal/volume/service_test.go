package volume

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/testutil"
)

// mockVolEngine is a configurable VolEngine that records its call sequence.
type mockVolEngine struct {
	mu    sync.Mutex
	calls []string

	// archiveFn writes the archive payload to out (defaults to a fixed payload).
	archiveFn func(out io.Writer) error
	// restoreFn consumes the restore stream (defaults to draining into restored).
	restoreFn func(in io.Reader) error
	restored  []byte

	// scaleErr lets the test fail the first N ServiceScale(replicas>0) calls.
	scaleUpFailFirst int
	scaleErr         error

	// state is returned by ServiceState; defaults to Running:0.
	state docker.ServiceState
}

func (m *mockVolEngine) record(s string) {
	m.mu.Lock()
	m.calls = append(m.calls, s)
	m.mu.Unlock()
}

func (m *mockVolEngine) callSeq() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.calls))
	copy(out, m.calls)
	return out
}

func (m *mockVolEngine) VolumeArchive(_ context.Context, _ string, out io.Writer) error {
	m.record("archive")
	if m.archiveFn != nil {
		return m.archiveFn(out)
	}
	_, err := out.Write([]byte("PAYLOAD-123"))
	return err
}

func (m *mockVolEngine) VolumeRestore(_ context.Context, _ string, in io.Reader) error {
	m.record("restore")
	if m.restoreFn != nil {
		return m.restoreFn(in)
	}
	b, err := io.ReadAll(in)
	m.mu.Lock()
	m.restored = b
	m.mu.Unlock()
	return err
}

func (m *mockVolEngine) ServiceScale(_ context.Context, name string, replicas uint64) error {
	m.mu.Lock()
	if replicas == 0 {
		m.calls = append(m.calls, "scale0")
	} else {
		m.calls = append(m.calls, "scaleUp")
		if m.scaleUpFailFirst > 0 {
			m.scaleUpFailFirst--
			m.mu.Unlock()
			if m.scaleErr != nil {
				return m.scaleErr
			}
			return errors.New("scale up failed")
		}
	}
	m.mu.Unlock()
	return nil
}

func (m *mockVolEngine) ServiceState(_ context.Context, _ string) (docker.ServiceState, error) {
	m.record("state")
	return m.state, nil
}

// mockVolStore is an in-memory VolumeStore for service tests.
type mockVolStore struct {
	backups map[int64]VolBackupRow
	target  VolTarget
	dst     backup.Destination

	mu      sync.Mutex
	results []string // status values recorded via SetVolumeBackupResult
}

func (s *mockVolStore) GetVolumeBackup(_ context.Context, id int64) (VolBackupRow, error) {
	b, ok := s.backups[id]
	if !ok {
		return VolBackupRow{}, errors.New("no such backup")
	}
	return b, nil
}

func (s *mockVolStore) GetVolTarget(_ context.Context, _ int64) (VolTarget, error) {
	return s.target, nil
}

func (s *mockVolStore) GetDestination(_ context.Context, _ int64) (backup.Destination, error) {
	return s.dst, nil
}

func (s *mockVolStore) SetVolumeBackupResult(_ context.Context, _ int64, _ time.Time, status, _ string) error {
	s.mu.Lock()
	s.results = append(s.results, status)
	s.mu.Unlock()
	return nil
}

func (s *mockVolStore) ListEnabledBackups(_ context.Context) ([]backup.SchedBackup, error) {
	out := make([]backup.SchedBackup, 0, len(s.backups))
	for id := range s.backups {
		out = append(out, backup.SchedBackup{ID: id})
	}
	return out, nil
}

// newMinioStore spins a MinIO container, creates a bucket and returns a store
// wired to a single backup id with the given prefix/retention.
func newMinioStore(t *testing.T, prefix string, retention int) (*mockVolStore, backup.Destination) {
	t.Helper()
	m := testutil.NewMinio(t)
	dst := backup.Destination{Endpoint: m.Endpoint, Bucket: "test", Region: m.Region, AccessKey: m.AccessKey, SecretKey: m.SecretKey}
	if err := backup.CreateBucket(context.Background(), dst, true); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	store := &mockVolStore{
		backups: map[int64]VolBackupRow{
			1: {ID: 1, AppVolumeID: 10, DestinationID: 100, Prefix: prefix, Retention: retention},
		},
		target: VolTarget{AppVolumeID: 10, AppID: 42, VolumeName: "data", ServiceName: docker.ServiceName(42), Replicas: 2},
		dst:    dst,
	}
	return store, dst
}

func TestRunVolumeBackupRoundtrip(t *testing.T) {
	store, dst := newMinioStore(t, "vol", 7)
	eng := &mockVolEngine{} // default archive writes PAYLOAD-123, state Running:0
	svc := New(eng, store, true)
	ctx := context.Background()

	if err := svc.RunVolumeBackup(ctx, 1, time.Now()); err != nil {
		t.Fatalf("RunVolumeBackup: %v", err)
	}

	// Exactly one object under the backup's prefix.
	objs, err := svc.ListObjects(ctx, 1)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("want 1 object, got %d: %+v", len(objs), objs)
	}
	wantPrefix := prefixDir("vol", volKey(42, "data"))
	if got := objs[0].Key; len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("object key %q not under prefix %q", got, wantPrefix)
	}

	// Drive a restore: the mock VolumeRestore drains the (gunzipped) stream.
	if err := svc.RestoreByID(ctx, 1, objs[0].Key); err != nil {
		t.Fatalf("RestoreByID: %v", err)
	}
	eng.mu.Lock()
	restored := string(eng.restored)
	eng.mu.Unlock()
	if restored != "PAYLOAD-123" {
		t.Fatalf("restored payload = %q, want PAYLOAD-123 (gzip+pipe+S3 roundtrip broken)", restored)
	}

	_ = dst
}

func TestRunVolumeBackupRetention(t *testing.T) {
	store, _ := newMinioStore(t, "vol", 2)
	eng := &mockVolEngine{}
	svc := New(eng, store, true)
	ctx := context.Background()

	// 4 runs with distinct timestamps so the S3 keys differ. Space the uploads
	// so each object gets a distinct LastModified (MinIO granularity is ~1s).
	base := time.Date(2026, 6, 15, 1, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		if err := svc.RunVolumeBackup(ctx, 1, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		time.Sleep(1100 * time.Millisecond)
	}

	objs, err := svc.ListObjects(ctx, 1)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(objs) != 2 {
		var keys []string
		for _, o := range objs {
			keys = append(keys, o.Key)
		}
		t.Fatalf("want 2 objects after retention=2, got %d: %v", len(objs), keys)
	}
	// The two survivors must be the newest two (by key timestamp): minutes 2 and 3.
	for _, o := range objs {
		if !contains(o.Key, "01-02-00Z") && !contains(o.Key, "01-03-00Z") {
			t.Fatalf("unexpected surviving object %q (oldest should have been pruned)", o.Key)
		}
	}
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

func TestVolumeBackupKeyContainment(t *testing.T) {
	store, _ := newMinioStore(t, "vol", 7)
	eng := &mockVolEngine{}
	svc := New(eng, store, true)
	ctx := context.Background()

	// A key that is NOT under prefixDir("vol", "42-data").
	badKey := "vol/999-other/2026-06-15T00-00-00Z.tar.gz"

	if _, err := svc.OpenObject(ctx, 1, badKey); !errors.Is(err, ErrKeyOutsideVolumeBackup) {
		t.Fatalf("OpenObject(badKey) err = %v, want ErrKeyOutsideVolumeBackup", err)
	}
	if err := svc.RestoreByID(ctx, 1, badKey); !errors.Is(err, ErrKeyOutsideVolumeBackup) {
		t.Fatalf("RestoreByID(badKey) err = %v, want ErrKeyOutsideVolumeBackup", err)
	}
	// Restore must not have touched the engine at all.
	if len(eng.callSeq()) != 0 {
		t.Fatalf("engine should not be called on a contained-out key, got %v", eng.callSeq())
	}
}

func TestRestoreQuiescesAndScalesBack(t *testing.T) {
	store, _ := newMinioStore(t, "vol", 7)
	// First, write a real archive object to S3 so the restore download succeeds.
	eng := &mockVolEngine{}
	svc := New(eng, store, true)
	ctx := context.Background()
	if err := svc.RunVolumeBackup(ctx, 1, time.Now()); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	objs, err := svc.ListObjects(ctx, 1)
	if err != nil || len(objs) != 1 {
		t.Fatalf("seed list: %v objs=%d", err, len(objs))
	}
	key := objs[0].Key

	// Fresh engine to record the restore call order cleanly.
	eng2 := &mockVolEngine{} // state Running:0 → restore proceeds immediately
	svc2 := New(eng2, store, true)
	if err := svc2.RestoreByID(ctx, 1, key); err != nil {
		t.Fatalf("RestoreByID: %v", err)
	}
	seq := eng2.callSeq()
	// Expect: scale0, (one or more) state, restore, scaleUp.
	assertOrder(t, seq, "scale0", "restore", "scaleUp")

	// Variant: the scale-up's first two attempts fail, then succeed (defer retries).
	eng3 := &mockVolEngine{scaleUpFailFirst: 2}
	svc3 := New(eng3, store, true)
	if err := svc3.RestoreByID(ctx, 1, key); err != nil {
		t.Fatalf("RestoreByID (retry variant): %v", err)
	}
	seq3 := eng3.callSeq()
	scaleUps := 0
	for _, c := range seq3 {
		if c == "scaleUp" {
			scaleUps++
		}
	}
	if scaleUps != 3 {
		t.Fatalf("expected 3 scaleUp attempts (2 fail + 1 success), got %d in %v", scaleUps, seq3)
	}
	assertOrder(t, seq3, "scale0", "restore", "scaleUp")
}

// assertOrder checks that each step appears in seq in the given relative order.
func assertOrder(t *testing.T, seq []string, steps ...string) {
	t.Helper()
	idx := 0
	for _, c := range seq {
		if idx < len(steps) && c == steps[idx] {
			idx++
		}
	}
	if idx != len(steps) {
		t.Fatalf("call order %v does not contain %v in order (matched %d)", seq, steps, idx)
	}
}

func TestRunVolumeBackupInFlight(t *testing.T) {
	store, _ := newMinioStore(t, "vol", 7)

	block := make(chan struct{})
	started := make(chan struct{})
	var startOnce, blockOnce sync.Once
	eng := &mockVolEngine{
		archiveFn: func(out io.Writer) error {
			startOnce.Do(func() { close(started) }) // signal in-flight (first call only)
			blockOnce.Do(func() { <-block })        // hold the FIRST backup in-flight only
			_, err := out.Write([]byte("PAYLOAD-123"))
			return err
		},
	}
	svc := New(eng, store, true)
	ctx := context.Background()

	done := make(chan error, 1)
	go func() { done <- svc.RunVolumeBackup(ctx, 1, time.Now()) }()

	<-started // first run is now in-flight (inside VolumeArchive)
	// A second run of the SAME id must be rejected.
	if err := svc.RunVolumeBackup(ctx, 1, time.Now()); !errors.Is(err, ErrVolumeBackupRunning) {
		t.Fatalf("second concurrent run err = %v, want ErrVolumeBackupRunning", err)
	}

	close(block) // let the first finish
	if err := <-done; err != nil {
		t.Fatalf("first run: %v", err)
	}

	// After it finished, a fresh run is allowed again.
	if err := svc.RunVolumeBackup(ctx, 1, time.Now()); err != nil {
		t.Fatalf("post-finish run: %v", err)
	}
}
