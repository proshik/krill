package statebackup

import (
	"bytes"
	"compress/gzip"
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"

	"github.com/proshik/krill/internal/backup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/netguard"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/testutil"
)

const (
	testDSN      = "postgres://krill:pw-SECRET@db.example:5432/krill?sslmode=disable"
	testS3Secret = "s3-SECRET"
	dumpDone     = "-- PostgreSQL database dump complete\n"
)

// fakeRunner records every spec it is given and what it read from the spec's
// stdin. By default it writes emit to out and returns err; fn, when set,
// replaces that behaviour. Like the real wrapper (dumpScript), it then prints
// the run's completion marker if the dump succeeded, unless raw is set, in
// which case emit/fn's output is the whole stream.
type fakeRunner struct {
	mu     sync.Mutex
	specs  []docker.OneOffSpec
	stdins []string
	emit   []byte
	err    error
	raw    bool
	fn     func(ctx context.Context, spec docker.OneOffSpec, out io.Writer) error
	// rm, when set, answers RemoveContainersByLabel; rmCalls records each
	// call's key=value.
	rm      func(ctx context.Context) (int, error)
	rmCalls []string
}

func (f *fakeRunner) RunOneOff(ctx context.Context, spec docker.OneOffSpec, out io.Writer) error {
	var stdin []byte
	if spec.Stdin != nil {
		stdin, _ = io.ReadAll(spec.Stdin)
	}
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.stdins = append(f.stdins, string(stdin))
	fn, emit, err, raw := f.fn, f.emit, f.err, f.raw
	f.mu.Unlock()
	if fn != nil {
		err = fn(ctx, spec, out)
	} else if len(emit) > 0 {
		if _, werr := out.Write(emit); werr != nil {
			return werr
		}
	}
	if err != nil || raw {
		return err
	}
	_, err = io.WriteString(out, completionMarker(nonceOf(spec)))
	return err
}

// nonceOf is the marker nonce dumpCmd put into spec's command.
func nonceOf(spec docker.OneOffSpec) string {
	if len(spec.Cmd) < 5 {
		return ""
	}
	return spec.Cmd[4]
}

func (f *fakeRunner) stdinOf(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stdins[i]
}

func (f *fakeRunner) RemoveContainersByLabel(ctx context.Context, key, value string) (int, error) {
	f.mu.Lock()
	f.rmCalls = append(f.rmCalls, key+"="+value)
	rm := f.rm
	f.mu.Unlock()
	if rm != nil {
		return rm(ctx)
	}
	return 0, nil
}

func (f *fakeRunner) removeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rmCalls...)
}

func (f *fakeRunner) calls() []docker.OneOffSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]docker.OneOffSpec(nil), f.specs...)
}

// fakeQuerier answers SHOW server_version_num.
type fakeQuerier struct{ version string }

type fakeRow struct{ v string }

func (r fakeRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return fmt.Errorf("fakeRow: %d destinations", len(dest))
	}
	switch d := dest[0].(type) {
	case *string:
		*d = r.v
	default:
		return fmt.Errorf("fakeRow: unsupported destination %T", dest[0])
	}
	return nil
}

func (q fakeQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if !strings.Contains(sql, "server_version_num") {
		return fakeRow{v: "unexpected query"}
	}
	return fakeRow{v: q.version}
}

type failure struct{ trigger, from, to, reason string }

type recNotifier struct {
	mu    sync.Mutex
	calls []failure
}

func (n *recNotifier) StateBackupFailed(_ context.Context, trigger, from, to, reason string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, failure{trigger, from, to, reason})
}

func (n *recNotifier) got() []failure {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]failure(nil), n.calls...)
}

type fixture struct {
	svc    *Service
	st     *DBStore
	runner *fakeRunner
	notes  *recNotifier
	dst    backup.Destination // the configured storage (bucket created)
	id     *age.X25519Identity
}

// newFixture builds a Service on a real DBStore. With minio it also creates a
// "state" bucket of the test's own on the shared S3 server.
func newFixture(t *testing.T, dsn string, minio bool) *fixture {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		st:     NewDBStore(db.New(testutil.NewTestDB(t))),
		runner: &fakeRunner{emit: []byte(dumpDone)},
		notes:  &recNotifier{},
		id:     id,
	}
	if minio {
		m := testutil.NewMinio(t)
		f.dst = backup.Destination{Endpoint: m.Endpoint, Bucket: m.Bucket(t, "state"), Region: m.Region, AccessKey: m.AccessKey, SecretKey: m.SecretKey}
		if err := backup.CreateBucket(context.Background(), f.dst, true); err != nil {
			t.Fatalf("create bucket: %v", err)
		}
	} else {
		f.dst = backup.Destination{Endpoint: "http://127.0.0.1:1", Bucket: "state", Region: "us-east-1", AccessKey: "ak", SecretKey: testS3Secret}
	}
	f.svc = New(fakeQuerier{version: "160004"}, f.runner, f.st, dsn, true)
	f.svc.SetNotifier(f.notes)
	return f
}

// configure stores enabled settings for f.dst, bypassing Save's access check.
func (f *fixture) configure(t *testing.T, mutate func(*Settings)) {
	t.Helper()
	s := Settings{
		Endpoint: f.dst.Endpoint, Bucket: f.dst.Bucket, Region: f.dst.Region,
		AccessKey: f.dst.AccessKey, SecretKey: f.dst.SecretKey, HasSecretKey: true,
		Prefix: "ops", Recipient: f.id.Recipient().String(), Schedule: "0 3 * * *", Retention: 14,
	}
	if mutate != nil {
		mutate(&s)
	}
	ctx := context.Background()
	if err := f.st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) runs(t *testing.T) []Run {
	t.Helper()
	rs, err := f.st.ListRuns(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func (f *fixture) keys(t *testing.T, prefix string) []string {
	t.Helper()
	objs, err := backup.List(context.Background(), f.dst, prefix, true)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, o := range objs {
		out = append(out, o.Key)
	}
	return out
}

func (f *fixture) seed(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if err := backup.Upload(context.Background(), f.dst, k, strings.NewReader("old"), true); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fixture) decrypt(t *testing.T, key string) []byte {
	t.Helper()
	rc, err := backup.Download(context.Background(), f.dst, key, true)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	plain, err := age.Decrypt(rc, f.id)
	if err != nil {
		t.Fatalf("age decrypt: %v", err)
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	b, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func TestImageFor(t *testing.T) {
	for v, want := range map[int]string{170004: "postgres:17-alpine", 160010: "postgres:16-alpine", 130000: "postgres:13-alpine"} {
		if got, err := ImageFor(v); err != nil || got != want {
			t.Errorf("ImageFor(%d) = %q, %v; want %q", v, got, err, want)
		}
	}
	if _, err := ImageFor(120022); !errors.Is(err, ErrServerTooOld) {
		t.Errorf("ImageFor(120022): want ErrServerTooOld, got %v", err)
	}
}

func TestDumpArgs(t *testing.T) {
	want := []string{"pg_dump", "--format=plain", "--no-owner", "--no-privileges", "--lock-wait-timeout=60s",
		"--exclude-table-data=*.sessions", "--exclude-table-data=*.metric_samples"}
	if got := DumpArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DumpArgs = %q", got)
	}
}

func TestRunHappyPath(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	ctx := context.Background()
	if err := f.svc.Run(ctx, TriggerManual); err != nil {
		t.Fatalf("Run: %v", err)
	}

	keys := f.keys(t, "ops/")
	if len(keys) != 1 || !strings.HasPrefix(keys[0], ScheduledDir("ops")) || !scheduledRe.MatchString(strings.TrimPrefix(keys[0], ScheduledDir("ops"))) {
		t.Fatalf("objects = %q", keys)
	}
	if got := f.decrypt(t, keys[0]); string(got) != dumpDone {
		t.Fatalf("decrypted dump = %q", got)
	}

	specs := f.runner.calls()
	if len(specs) != 1 {
		t.Fatalf("runner calls = %d", len(specs))
	}
	sp := specs[0]
	if !sp.HostNetwork || sp.Image != "postgres:16-alpine" || !reflect.DeepEqual(sp.Cmd, dumpCmd(nonceOf(sp))) ||
		!reflect.DeepEqual(sp.Cmd[5:], DumpArgs()) || len(nonceOf(sp)) != 32 ||
		!reflect.DeepEqual(sp.Labels, map[string]string{"krill.role": "state-dump"}) {
		t.Errorf("spec = %+v", sp)
	}
	// The password reaches the container on stdin only: never in its Env,
	// argv or labels, which its config (docker inspect, a debug daemon log)
	// shows.
	if got := f.runner.stdinOf(0); got != "pw-SECRET\n" {
		t.Errorf("stdin = %q, want the password line", got)
	}
	for _, v := range append(append(append([]string{}, sp.Env...), sp.Cmd...), sp.Labels["krill.role"]) {
		if strings.Contains(v, "pw-SECRET") || strings.HasPrefix(v, "PGPASSWORD") {
			t.Errorf("password in the container's config: %q", v)
		}
	}

	runs := f.runs(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	r := runs[0]
	size, err := backup.Head(ctx, f.dst, keys[0], true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ok" || r.Trigger != TriggerManual || r.ObjectKey != keys[0] || r.SizeBytes != size || r.Error != "" {
		t.Errorf("run = %+v (object size %d)", r, size)
	}
	if n := f.notes.got(); len(n) != 0 {
		t.Errorf("notifier called: %+v", n)
	}
}

func TestRunSidecarErrorLeavesNoObject(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	f.runner.emit = bytes.Repeat([]byte("x"), 1024)
	f.runner.err = errors.New("one-off container exited 1: pg_dump: error: query failed")
	if err := f.svc.Run(context.Background(), TriggerManual); err == nil {
		t.Fatal("Run: want an error")
	}
	if keys := f.keys(t, ""); len(keys) != 0 {
		t.Fatalf("objects after a failed dump: %q", keys)
	}
	runs := f.runs(t)
	if len(runs) != 1 || runs[0].Status != "error" || !strings.Contains(runs[0].Error, "query failed") {
		t.Fatalf("runs = %+v", runs)
	}
	n := f.notes.got()
	if len(n) != 1 || n[0].trigger != "manual" || n[0].reason != runs[0].Error {
		t.Fatalf("notifications = %+v", n)
	}
}

func TestRunUploadErrorStopsRunner(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, func(s *Settings) { s.Bucket = "no-such-bucket" })
	done := make(chan struct{})
	f.runner.fn = func(_ context.Context, _ docker.OneOffSpec, out io.Writer) error {
		defer close(done)
		// Fresh pseudo-random data every chunk: a repeated 32 KiB chunk sits
		// inside deflate's window and compresses to nothing, so the uploader
		// would read the whole stream instead of failing on its first part.
		chunk := make([]byte, 32<<10)
		rnd := rand.New(rand.NewSource(1))
		for written := 0; written < 64<<20; written += len(chunk) {
			rnd.Read(chunk)
			if _, err := out.Write(chunk); err != nil {
				return err
			}
		}
		return nil
	}
	errCh := make(chan error, 1)
	go func() { errCh <- f.svc.Run(context.Background(), TriggerManual) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("runner still writing 20s after Run started: the pipe was not closed on the upload error")
	}
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "upload failed") {
			t.Fatalf("Run into a missing bucket: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the runner stopped")
	}
	if runs := f.runs(t); len(runs) != 1 || runs[0].Status != "error" {
		t.Fatalf("runs = %+v", runs)
	}
}

// A dump whose output does not end with this run's completion marker (an
// attach stream that ended early, even at a line that looks like pg_dump's
// footer) must not leave an object, even though the runner succeeded.
func TestRunWithoutMarkerLeavesNoObject(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	f.runner.raw = true
	body := "--\n-- PostgreSQL database dump\n--\n\nCREATE FUNCTION f() RETURNS text AS $$\n" + dumpDone
	cases := map[string]func(nonce string) string{
		"marker missing":             func(string) string { return body },
		"empty stream":               func(string) string { return "" },
		"wrong nonce":                func(string) string { return body + completionMarker(strings.Repeat("0", 32)) },
		"marker not at the end":      func(n string) string { return body + completionMarker(n) + "more SQL;\n" },
		"data after the marker":      func(n string) string { return body + completionMarker(n) + "x" },
		"marker without its newline": func(n string) string { return body + strings.TrimSuffix(completionMarker(n), "\n") },
		"marker cut short":           func(n string) string { return body + completionMarker(n)[:20] },
		"marker inside a line":       func(n string) string { return body + "x" + strings.TrimPrefix(completionMarker(n), "\n") },
	}
	for name, emit := range cases {
		f.runner.fn = func(_ context.Context, spec docker.OneOffSpec, out io.Writer) error {
			_, err := io.WriteString(out, emit(nonceOf(spec)))
			return err
		}
		err := f.svc.Run(context.Background(), TriggerManual)
		if err == nil || !strings.Contains(err.Error(), errIncompleteDump.Error()) {
			t.Fatalf("%s: Run = %v", name, err)
		}
		if keys := f.keys(t, ""); len(keys) != 0 {
			t.Fatalf("%s: objects after an incomplete dump: %q", name, keys)
		}
	}
	runs := f.runs(t)
	if len(runs) != len(cases) || runs[0].Status != "error" || runs[0].Error != errIncompleteDump.Error() {
		t.Fatalf("runs = %+v", runs)
	}
}

// Each run gets its own nonce: a marker from an earlier run (say, inside the
// dumped data) never completes a later one.
func TestRunNonceIsPerRun(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	for range 2 {
		if err := f.svc.Run(context.Background(), TriggerManual); err != nil {
			t.Fatal(err)
		}
	}
	specs := f.runner.calls()
	if len(specs) != 2 || nonceOf(specs[0]) == nonceOf(specs[1]) {
		t.Fatalf("nonces: %q", []string{nonceOf(specs[0]), nonceOf(specs[1])})
	}
}

// Two runs in the same second must not share an object key: the second one
// waits for the next second instead of overwriting the first one's backup.
func TestRunsInOneSecondGetDistinctKeys(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	base := time.Now().Truncate(time.Second).Add(900 * time.Millisecond)
	var mu sync.Mutex
	calls := 0
	f.svc.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 2 { // both runs start within one second
			return base
		}
		return base.Add(200 * time.Millisecond)
	}
	for range 2 {
		if err := f.svc.Run(context.Background(), TriggerManual); err != nil {
			t.Fatal(err)
		}
	}
	runs := f.runs(t)
	if len(runs) != 2 || runs[0].ObjectKey == runs[1].ObjectKey {
		t.Fatalf("runs = %+v", runs)
	}
	if keys := f.keys(t, ScheduledDir("ops")); len(keys) != 2 {
		t.Fatalf("objects = %q", keys)
	}
	if got, want := runs[0].ObjectKey, ScheduledKey("ops", base.Add(200*time.Millisecond)); got != want {
		t.Fatalf("second run's key = %q, want %q", got, want)
	}
}

// A run waiting for the next second gives up with its context.
func TestKeyTimeHonorsContext(t *testing.T) {
	s := &Service{now: func() time.Time { return time.Date(2026, 11, 20, 10, 0, 0, 500, time.UTC) }}
	if _, err := s.keyTime(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.keyTime(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("keyTime with a cancelled context = %v", err)
	}
}

// A DSN without a password sends an empty line; the wrapper then exports no
// PGPASSWORD at all.
func TestRunWithoutPassword(t *testing.T) {
	f := newFixture(t, "postgres://krill@db.example:5432/krill?sslmode=disable", true)
	f.configure(t, nil)
	if err := f.svc.Run(context.Background(), TriggerManual); err != nil {
		t.Fatal(err)
	}
	if got := f.runner.stdinOf(0); got != "\n" {
		t.Fatalf("stdin = %q", got)
	}
}

// The marker is held back at every split of the stream, so the archive is
// exactly what pg_dump wrote.
func TestMarkerTailSplits(t *testing.T) {
	marker := completionMarker("abc")
	dump := "SELECT 1;\n" + dumpDone
	stream := dump + marker
	for chunk := 1; chunk <= len(stream)+1; chunk++ {
		var got bytes.Buffer
		tail := newMarkerTail(&got, marker)
		for i := 0; i < len(stream); i += chunk {
			if _, err := tail.Write([]byte(stream[i:min(i+chunk, len(stream))])); err != nil {
				t.Fatal(err)
			}
		}
		if !tail.complete() || got.String() != dump {
			t.Fatalf("chunk %d: complete=%v archive=%q", chunk, tail.complete(), got.String())
		}
	}
	tail := newMarkerTail(io.Discard, marker)
	_, _ = tail.Write([]byte(marker[:len(marker)-1]))
	if tail.complete() {
		t.Fatal("a cut marker completed the dump")
	}
}

// What current pg_dump prints after the footer must not fail the check.
func TestRunAcceptsTextAfterFooter(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	emit := "--\n-- PostgreSQL database dump\n--\n\n\\restrict k3y\n\n--\n" + dumpDone + "--\n\n\\unrestrict k3y\n\n"
	f.runner.emit = []byte(emit)
	if err := f.svc.Run(context.Background(), TriggerManual); err != nil {
		t.Fatalf("Run: %v", err)
	}
	keys := f.keys(t, "ops/")
	if len(keys) != 1 {
		t.Fatalf("objects = %q", keys)
	}
	if got := f.decrypt(t, keys[0]); string(got) != emit {
		t.Fatalf("decrypted dump = %q", got)
	}
}

func TestRunMultipartSizeCheck(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	payload := make([]byte, 12<<20)
	if _, err := crand.Read(payload); err != nil {
		t.Fatal(err)
	}
	f.runner.emit = payload
	ctx := context.Background()
	if err := f.svc.Run(ctx, TriggerSchedule); err != nil {
		t.Fatalf("Run: %v", err)
	}
	keys := f.keys(t, ScheduledDir("ops"))
	if len(keys) != 1 {
		t.Fatalf("objects = %q", keys)
	}
	size, err := backup.Head(ctx, f.dst, keys[0], true)
	if err != nil {
		t.Fatal(err)
	}
	runs := f.runs(t)
	if len(runs) != 1 || runs[0].Status != "ok" || runs[0].SizeBytes != size || size <= 12<<20 {
		t.Fatalf("runs = %+v, object size %d", runs, size)
	}
	if !bytes.Equal(f.decrypt(t, keys[0]), payload) {
		t.Fatal("decrypted payload differs")
	}
}

// The size check runs against what the storage reports: a mismatch fails the
// run, deletes the new object and leaves the older recovery points alone.
func TestRunSizeMismatchDeletesNewObject(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, func(s *Settings) { s.Retention = 1 })
	older := []string{ScheduledKey("ops", t0.Add(-2*time.Hour)), ScheduledKey("ops", t0.Add(-time.Hour))}
	f.seed(t, older...)
	var headed []string
	f.svc.head = func(ctx context.Context, d backup.Destination, key string, allowPrivate bool) (int64, error) {
		headed = append(headed, key)
		n, err := backup.Head(ctx, d, key, allowPrivate)
		return n + 1, err
	}
	err := f.svc.Run(context.Background(), TriggerSchedule)
	if err == nil || !strings.Contains(err.Error(), "were sent; it was deleted") {
		t.Fatalf("Run = %v", err)
	}
	keys := f.keys(t, ScheduledDir("ops"))
	if len(headed) != 1 || len(keys) != 2 || keys[0] == headed[0] || keys[1] == headed[0] {
		t.Fatalf("objects after a size mismatch: %q (checked %q)", keys, headed)
	}
	for _, k := range older {
		if k != keys[0] && k != keys[1] {
			t.Fatalf("older recovery point %q is gone: %q", k, keys)
		}
	}
	runs := f.runs(t)
	if len(runs) != 1 || runs[0].Status != "error" || runs[0].ObjectKey != "" {
		t.Fatalf("runs = %+v", runs)
	}
	if n := f.notes.got(); len(n) != 1 {
		t.Fatalf("notifications = %+v", n)
	}
}

// A check that used up the run's deadline still has its object deleted: the
// cleanup does not inherit the expired context.
func TestRunCheckTimeoutStillDeletesObject(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.svc.head = func(hctx context.Context, _ backup.Destination, _ string, _ bool) (int64, error) {
		cancel() // the run's deadline passes during the check
		<-hctx.Done()
		return 0, hctx.Err()
	}
	err := f.svc.Run(ctx, TriggerManual)
	if err == nil || !strings.Contains(err.Error(), "could not be checked") || !strings.Contains(err.Error(), "it was deleted") {
		t.Fatalf("Run = %v", err)
	}
	if keys := f.keys(t, ""); len(keys) != 0 {
		t.Fatalf("an unverified object was left: %q", keys)
	}
}

func TestRunRetention(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, func(s *Settings) { s.Retention = 2 })
	old := []string{
		ScheduledKey("ops", t0.Add(-3*time.Hour)),
		ScheduledKey("ops", t0.Add(-2*time.Hour)),
		ScheduledKey("ops", t0.Add(-1*time.Hour)),
	}
	pre := PreUpdateKey("ops", t0, "v0.9.2", "v0.9.3")
	stray := ScheduledDir("ops") + "notes.txt"
	f.seed(t, append(append([]string{}, old...), pre, stray)...)

	if err := f.svc.Run(context.Background(), TriggerSchedule); err != nil {
		t.Fatalf("Run: %v", err)
	}
	keys := f.keys(t, "ops/")
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	if len(keys) != 4 || !set[old[2]] || !set[pre] || !set[stray] || set[old[0]] || set[old[1]] {
		t.Fatalf("after retention: %q", keys)
	}
	runs := f.runs(t)
	if len(runs) != 1 || runs[0].Status != "ok" || runs[0].Error != "" || !set[runs[0].ObjectKey] {
		t.Fatalf("runs = %+v", runs)
	}
}

// After the clock was set back, the new backup sorts behind later-dated
// ones; retention must still keep it (and N-1 others), not delete it.
func TestRunRetentionKeepsCurrentAfterClockSetBack(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, func(s *Settings) { s.Retention = 2 })
	future := time.Now().AddDate(1, 0, 0)
	later := []string{
		ScheduledKey("ops", future),
		ScheduledKey("ops", future.Add(time.Hour)),
		ScheduledKey("ops", future.Add(2*time.Hour)),
	}
	f.seed(t, later...)
	if err := f.svc.Run(context.Background(), TriggerSchedule); err != nil {
		t.Fatalf("Run: %v", err)
	}
	runs := f.runs(t)
	if len(runs) != 1 || runs[0].Status != "ok" || runs[0].Error != "" {
		t.Fatalf("runs = %+v", runs)
	}
	keys := f.keys(t, ScheduledDir("ops"))
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	if len(keys) != 2 || !set[runs[0].ObjectKey] || !set[later[2]] {
		t.Fatalf("after retention: %q; want the run's own %q and the newest other", keys, runs[0].ObjectKey)
	}
}

// TestPreUpdateKeysArePrunable pins that what Snapshot writes is what
// pre-update retention prunes.
func TestPreUpdateKeysArePrunable(t *testing.T) {
	var objs []backup.Object
	for i := range PreUpdateKeep + 2 {
		objs = append(objs, backup.Object{Key: PreUpdateKey("ops", t0.Add(time.Duration(i)*time.Hour), "v0.9.3", "v0.9.4")})
	}
	got := Prunable(objs, PreUpdateDir("ops"), true, PreUpdateKeep, "")
	if len(got) != 2 || got[0].Key != objs[1].Key || got[1].Key != objs[0].Key {
		t.Fatalf("Prunable = %+v", got)
	}
}

func TestSnapshotNaming(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	var seeded []string
	for i := range 6 {
		// Older than the snapshot about to be taken.
		seeded = append(seeded, PreUpdateKey("ops", t0.AddDate(0, -1, 0).Add(time.Duration(i)*time.Hour), "v0.9.1", "v0.9.2"))
	}
	f.seed(t, seeded...)

	took, err := f.svc.Snapshot(context.Background(), "v0.9.3", "v0.9.4")
	if !took || err != nil {
		t.Fatalf("Snapshot = %v, %v", took, err)
	}
	runs := f.runs(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	r := runs[0]
	if r.Status != "ok" || r.Trigger != TriggerPreUpdate || r.FromVersion != "v0.9.3" || r.ToVersion != "v0.9.4" {
		t.Fatalf("run = %+v", r)
	}
	want := PreUpdateKey("ops", r.StartedAt, "v0.9.3", "v0.9.4")
	if r.ObjectKey != want {
		t.Fatalf("object key = %q, want %q", r.ObjectKey, want)
	}
	keys := f.keys(t, PreUpdateDir("ops"))
	if len(keys) != PreUpdateKeep {
		t.Fatalf("pre-update objects = %q", keys)
	}
	found := false
	for _, k := range keys {
		found = found || k == want
	}
	if !found {
		t.Fatalf("snapshot %q not among %q", want, keys)
	}
	for _, k := range keys {
		if k == seeded[0] || k == seeded[1] {
			t.Fatalf("an oldest pre-update dump survived: %q", keys)
		}
	}
	if got := f.decrypt(t, want); string(got) != dumpDone {
		t.Fatalf("decrypted = %q", got)
	}
}

func TestSnapshotNotConfigured(t *testing.T) {
	f := newFixture(t, testDSN, false)
	ctx := context.Background()
	if took, err := f.svc.Snapshot(ctx, "v1.0.0", "v1.0.1"); took || err != nil {
		t.Fatalf("no settings: Snapshot = %v, %v", took, err)
	}
	f.configure(t, nil)
	if err := f.st.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if took, err := f.svc.Snapshot(ctx, "v1.0.0", "v1.0.1"); took || err != nil {
		t.Fatalf("paused: Snapshot = %v, %v", took, err)
	}
	if n := len(f.runner.calls()); n != 0 {
		t.Fatalf("runner called %d times", n)
	}
	if runs := f.runs(t); len(runs) != 0 {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestSnapshotUndecryptableBlocks(t *testing.T) {
	secret.Init("key-one")
	defer secret.Init("")
	f := newFixture(t, testDSN, false)
	f.configure(t, nil)
	secret.Init("key-two") // a rotated KRILL_SECRET_KEY

	took, err := f.svc.Snapshot(context.Background(), "v1.0.0", "v1.0.1")
	if took || !errors.Is(err, secret.ErrUndecryptable) {
		t.Fatalf("Snapshot = %v, %v; want (false, ErrUndecryptable)", took, err)
	}
	if !strings.Contains(err.Error(), "KRILL_SECRET_KEY") {
		t.Errorf("error = %q", err)
	}
	runs := f.runs(t)
	if len(runs) != 1 || runs[0].Status != "error" || runs[0].Trigger != TriggerPreUpdate || runs[0].Error != err.Error() {
		t.Fatalf("runs = %+v", runs)
	}
	if n := len(f.runner.calls()); n != 0 {
		t.Fatalf("runner called %d times", n)
	}
	if n := f.notes.got(); len(n) != 1 || n[0].trigger != "pre_update" || n[0].from != "v1.0.0" || n[0].to != "v1.0.1" {
		t.Fatalf("notifications = %+v", n)
	}
}

func TestClaim(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	entered, gate := make(chan struct{}), make(chan struct{})
	f.runner.fn = func(_ context.Context, _ docker.OneOffSpec, out io.Writer) error {
		close(entered)
		<-gate
		_, err := out.Write([]byte(dumpDone))
		return err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- f.svc.Run(context.Background(), TriggerManual) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("run never reached the runner")
	}

	if err := f.svc.Start(TriggerManual); !errors.Is(err, ErrStateBackupRunning) {
		t.Errorf("Start while running: %v", err)
	}
	if took, err := f.svc.Snapshot(context.Background(), "v1.0.0", "v1.0.1"); took || !errors.Is(err, ErrStateBackupRunning) {
		t.Errorf("Snapshot while running: %v, %v", took, err)
	}
	if err := f.svc.Run(context.Background(), TriggerSchedule); !errors.Is(err, ErrStateBackupRunning) {
		t.Errorf("Run while running: %v", err)
	}
	if n := f.svc.InFlight(); n != 1 {
		t.Errorf("InFlight = %d", n)
	}

	close(gate)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not finish")
	}
	if n := f.svc.InFlight(); n != 0 {
		t.Errorf("InFlight after the run = %d", n)
	}
	// Refused attempts wrote nothing.
	if runs := f.runs(t); len(runs) != 1 || runs[0].Status != "ok" {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestStartRunsInBackground(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	if err := f.svc.Start(TriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for f.svc.InFlight() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("background run did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if runs := f.runs(t); len(runs) != 1 || runs[0].Status != "ok" || runs[0].Trigger != TriggerManual {
		t.Fatalf("runs = %+v", runs)
	}
}

// flakyViewStore fails LoadView from the failFrom-th call on.
type flakyViewStore struct {
	*DBStore
	mu       sync.Mutex
	calls    int
	failFrom int
}

func (s *flakyViewStore) LoadView(ctx context.Context) (Settings, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if n >= s.failFrom {
		return Settings{}, errors.New("database is restarting")
	}
	return s.DBStore.LoadView(ctx)
}

// A run Start admitted whose settings re-read fails is a failed run: it is
// journalled and alerted, not lost behind the "started" confirmation.
func TestStartedRunSettingsErrorIsJournalled(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	f.svc.store = &flakyViewStore{DBStore: f.st, failFrom: 2}
	if err := f.svc.Start(TriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for f.svc.InFlight() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("background run did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	runs := f.runs(t)
	if len(runs) != 1 || runs[0].Status != "error" || runs[0].Error != errSettings.Error() || runs[0].Trigger != TriggerManual {
		t.Fatalf("runs = %+v", runs)
	}
	if n := f.notes.got(); len(n) != 1 || n[0].reason != errSettings.Error() {
		t.Fatalf("notifications = %+v", n)
	}
	if n := len(f.runner.calls()); n != 0 {
		t.Fatalf("runner called %d times", n)
	}
}

// The updater's last busy check runs after its own snapshot; a manual or
// scheduled run admitted after it would be cut short by the restart.
func TestStartRefusedWhileUpdating(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	f.svc.SetUpdateActive(func() bool { return true })
	for _, tr := range []Trigger{TriggerManual, TriggerSchedule} {
		if err := f.svc.Start(tr); !errors.Is(err, ErrUpdateInProgress) {
			t.Fatalf("Start(%s) during an update: %v", tr, err)
		}
		if err := f.svc.Run(context.Background(), tr); !errors.Is(err, ErrUpdateInProgress) {
			t.Fatalf("Run(%s) during an update: %v", tr, err)
		}
	}
	if n := f.svc.InFlight(); n != 0 {
		t.Fatalf("InFlight = %d: a refused run kept the slot", n)
	}
	if runs := f.runs(t); len(runs) != 0 || len(f.runner.calls()) != 0 {
		t.Fatalf("a refused run was journalled or dumped: %+v", runs)
	}
	// The update's own snapshot still runs.
	if took, err := f.svc.Snapshot(context.Background(), "v1.0.0", "v1.0.1"); !took || err != nil {
		t.Fatalf("Snapshot during the update = %v, %v", took, err)
	}
}

// A request refused during an update never shows as in flight: the
// updater's own busy check (InFlight) must not see a claim that is about to
// be refused. updateActive runs under the slot's mutex, so it reads the
// field directly.
func TestRefusedDuringUpdateNeverInFlight(t *testing.T) {
	f := newFixture(t, testDSN, false)
	f.configure(t, nil)
	var seen []bool
	f.svc.SetUpdateActive(func() bool {
		seen = append(seen, f.svc.inFlight)
		return true
	})
	if err := f.svc.Start(TriggerManual); !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("Start: %v", err)
	}
	if err := f.svc.Run(context.Background(), TriggerSchedule); !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) != 2 || seen[0] || seen[1] {
		t.Fatalf("slot taken while the update was asked = %v; want [false false]", seen)
	}
}

func TestRunNotConfigured(t *testing.T) {
	f := newFixture(t, testDSN, false)
	if err := f.svc.Start(TriggerManual); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Start: want ErrNotConfigured, got %v", err)
	}
	if err := f.svc.Run(context.Background(), TriggerManual); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Run: want ErrNotConfigured, got %v", err)
	}
	if n := f.svc.InFlight(); n != 0 {
		t.Fatalf("InFlight = %d", n)
	}
	if runs := f.runs(t); len(runs) != 0 {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestRunNoRunner(t *testing.T) {
	f := newFixture(t, testDSN, false)
	f.configure(t, nil)
	f.svc.runner = nil
	if err := f.svc.Run(context.Background(), TriggerManual); !errors.Is(err, ErrNoRunner) {
		t.Fatalf("Run: want ErrNoRunner, got %v", err)
	}
	if runs := f.runs(t); len(runs) != 1 || runs[0].Status != "error" {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestRunRejectsUnixSocketDSN(t *testing.T) {
	f := newFixture(t, "host=/var/run/postgresql user=krill password=pw-SECRET dbname=krill", false)
	f.configure(t, nil)
	if err := f.svc.Run(context.Background(), TriggerManual); !errors.Is(err, ErrUnixSocket) {
		t.Fatalf("Run: want ErrUnixSocket, got %v", err)
	}
	runs := f.runs(t)
	if len(runs) != 1 || runs[0].Status != "error" || !strings.Contains(runs[0].Error, "TCP connection") {
		t.Fatalf("runs = %+v", runs)
	}
	if n := len(f.runner.calls()); n != 0 {
		t.Fatalf("runner called %d times", n)
	}
}

func TestRunServerTooOld(t *testing.T) {
	f := newFixture(t, testDSN, false)
	f.configure(t, nil)
	f.svc.q = fakeQuerier{version: "120022"}
	if err := f.svc.Run(context.Background(), TriggerManual); !errors.Is(err, ErrServerTooOld) {
		t.Fatalf("Run: want ErrServerTooOld, got %v", err)
	}
	if runs := f.runs(t); len(runs) != 1 || runs[0].Status != "error" {
		t.Fatalf("runs = %+v", runs)
	}
}

// sslnegotiation=direct is libpq 17+: an older dump client would silently
// negotiate TLS the old way, so the run is refused before the dump starts.
func TestRunDirectNegotiationNeedsClient17(t *testing.T) {
	f := newFixture(t, "postgres://krill:pw-SECRET@db.example:5432/krill?sslmode=require&sslnegotiation=direct", false)
	f.configure(t, nil)
	if err := f.svc.Run(context.Background(), TriggerManual); !errors.Is(err, ErrClientTooOld) {
		t.Fatalf("Run: want ErrClientTooOld, got %v", err)
	}
	if runs := f.runs(t); len(runs) != 1 || runs[0].Status != "error" || !strings.Contains(runs[0].Error, "PostgreSQL 17") {
		t.Fatalf("runs = %+v", runs)
	}
	if n := len(f.runner.calls()); n != 0 {
		t.Fatalf("runner called %d times", n)
	}

	f.svc.q = fakeQuerier{version: "170002"}
	_ = f.svc.Run(context.Background(), TriggerManual)
	calls := f.runner.calls()
	if len(calls) != 1 || calls[0].Image != "postgres:17-alpine" || !contains(calls[0].Env, "PGSSLNEGOTIATION=direct") {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestRunErrorTextHasNoSecrets(t *testing.T) {
	check := func(t *testing.T, f *fixture, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("Run: want an error")
		}
		runs := f.runs(t)
		if len(runs) != 1 || runs[0].Status != "error" {
			t.Fatalf("runs = %+v", runs)
		}
		n := f.notes.got()
		if len(n) != 1 {
			t.Fatalf("notifications = %+v", n)
		}
		for what, text := range map[string]string{"returned": err.Error(), "journal": runs[0].Error, "alert": n[0].reason} {
			if strings.Contains(text, "pw-SECRET") || strings.Contains(text, testS3Secret) {
				t.Errorf("%s error carries a secret: %q", what, text)
			}
		}
	}

	t.Run("runner stderr", func(t *testing.T) {
		f := newFixture(t, testDSN, false)
		f.configure(t, nil)
		stderr := "pg_dump: error: connection to server failed: password pw-SECRET rejected, s3 " + testS3Secret + " " + strings.Repeat("e", 4096)
		f.runner.err = errors.New("one-off container exited 1: " + stderr)
		err := f.svc.Run(context.Background(), TriggerManual)
		check(t, f, err)
		if !strings.Contains(err.Error(), "connection to server failed") {
			t.Errorf("error lost the stderr: %q", err)
		}
		if len(err.Error()) > 1200 {
			t.Errorf("error is %d bytes: stderr was not capped", len(err.Error()))
		}
	})

	t.Run("wrong S3 secret", func(t *testing.T) {
		f := newFixture(t, testDSN, true)
		f.configure(t, func(s *Settings) { s.SecretKey = testS3Secret })
		check(t, f, f.svc.Run(context.Background(), TriggerManual))
	})
}

func TestSaveChecksAccess(t *testing.T) {
	f := newFixture(t, testDSN, true)
	changes := 0
	f.svc.SetOnChange(func() { changes++ })
	ctx := context.Background()
	in := Input{
		Endpoint: f.dst.Endpoint, Bucket: "no-such-bucket", Region: f.dst.Region,
		AccessKey: f.dst.AccessKey, SecretKey: f.dst.SecretKey, Prefix: "ops",
		Recipient: f.id.Recipient().String(), Preset: "daily", Retention: 14,
	}
	if err := f.svc.Save(ctx, in); err == nil {
		t.Fatal("Save with a missing bucket: want an error")
	}
	if v, err := f.svc.View(ctx); err != nil || v.Bucket != "" || changes != 0 {
		t.Fatalf("after a refused save: %+v, %v, changes=%d", v, err, changes)
	}

	in.Bucket = f.dst.Bucket
	if err := f.svc.Save(ctx, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	v, err := f.svc.View(ctx)
	if err != nil || v.Bucket != f.dst.Bucket || !v.HasSecretKey || v.Enabled || changes != 1 {
		t.Fatalf("after save: %+v, %v, changes=%d", v, err, changes)
	}

	if err := f.svc.SetEnabled(ctx, true); err != nil || changes != 2 {
		t.Fatalf("SetEnabled: %v, changes=%d", err, changes)
	}
}

func TestSaveUndecryptableAsksForKey(t *testing.T) {
	secret.Init("key-one")
	defer secret.Init("")
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	secret.Init("key-two")
	in := Input{
		Endpoint: f.dst.Endpoint, Bucket: f.dst.Bucket, Region: f.dst.Region, AccessKey: f.dst.AccessKey,
		Prefix: "ops", Recipient: f.id.Recipient().String(), Preset: "daily", Retention: 14,
	}
	if err := f.svc.Save(context.Background(), in); !errors.Is(err, ErrSecretKeyRequired) {
		t.Fatalf("Save keeping an undecryptable key: want ErrSecretKeyRequired, got %v", err)
	}
	in.SecretKey = f.dst.SecretKey
	if err := f.svc.Save(context.Background(), in); err != nil {
		t.Fatalf("Save with the key entered again: %v", err)
	}
}

func TestSchedStore(t *testing.T) {
	f := newFixture(t, testDSN, false)
	f.configure(t, func(s *Settings) { s.Schedule = "0 3 * * *" })
	ctx := context.Background()
	got, err := f.svc.SchedStore().ListEnabledBackups(ctx)
	if err != nil || len(got) != 1 || got[0].ID != 1 || got[0].Schedule != "0 3 * * *" {
		t.Fatalf("enabled: %+v, %v", got, err)
	}
	if err := f.svc.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got, err := f.svc.SchedStore().ListEnabledBackups(ctx); err != nil || len(got) != 0 {
		t.Fatalf("paused: %+v, %v", got, err)
	}
}

func TestNextRun(t *testing.T) {
	now := time.Date(2026, 11, 20, 2, 0, 0, 0, time.Local)
	got, ok := NextRun("0 3 * * *", now)
	if want := time.Date(2026, 11, 20, 3, 0, 0, 0, time.Local); !ok || !got.Equal(want) {
		t.Fatalf("NextRun = %v, %v; want %v", got, ok, want)
	}
	if _, ok := NextRun("", now); ok {
		t.Fatal("NextRun(\"\") reported a run")
	}
	if _, ok := NextRun("not a cron", now); ok {
		t.Fatal("NextRun(invalid) reported a run")
	}
}

func TestRunsAndLastSuccess(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	ctx := context.Background()
	if _, ok, err := f.svc.LastSuccess(ctx); ok || err != nil {
		t.Fatalf("LastSuccess before any run: %v, %v", ok, err)
	}
	if err := f.svc.Run(ctx, TriggerManual); err != nil {
		t.Fatal(err)
	}
	last, ok, err := f.svc.LastSuccess(ctx)
	if err != nil || !ok || last.Status != "ok" {
		t.Fatalf("LastSuccess = %+v, %v, %v", last, ok, err)
	}
	if rs, err := f.svc.Runs(ctx, 10); err != nil || len(rs) != 1 {
		t.Fatalf("Runs = %+v, %v", rs, err)
	}
}

// TestManualRunWhilePaused: pausing stops automatic runs only; "Back up now"
// still works.
func TestManualRunWhilePaused(t *testing.T) {
	f := newFixture(t, testDSN, true)
	f.configure(t, nil)
	ctx := context.Background()
	if err := f.svc.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Run(ctx, TriggerManual); err != nil {
		t.Fatalf("Run while paused: %v", err)
	}
	keys := f.keys(t, ScheduledDir("ops"))
	if len(keys) != 1 {
		t.Fatalf("objects = %q", keys)
	}
	runs := f.runs(t)
	if len(runs) != 1 || runs[0].Status != "ok" || runs[0].Trigger != TriggerManual || runs[0].ObjectKey != keys[0] {
		t.Fatalf("runs = %+v", runs)
	}

	// Start takes the same path.
	if err := f.svc.Start(TriggerManual); err != nil {
		t.Fatalf("Start while paused: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for f.svc.InFlight() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("background run did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if runs := f.runs(t); len(runs) != 2 || runs[0].Status != "ok" {
		t.Fatalf("runs after Start = %+v", runs)
	}
}

func TestScheduledRunWhilePausedRefused(t *testing.T) {
	f := newFixture(t, testDSN, false)
	f.configure(t, nil)
	ctx := context.Background()
	if err := f.svc.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Run(ctx, TriggerSchedule); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("scheduled Run while paused: want ErrNotConfigured, got %v", err)
	}
	if err := f.svc.Start(TriggerSchedule); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("scheduled Start while paused: want ErrNotConfigured, got %v", err)
	}
	if n := len(f.runner.calls()); n != 0 {
		t.Fatalf("runner called %d times", n)
	}
	if runs := f.runs(t); len(runs) != 0 {
		t.Fatalf("runs = %+v", runs)
	}
}

// TestSaveAccessErrorKeepsCause: the storage error is sanitized for the page
// but still matches its cause.
func TestSaveAccessErrorKeepsCause(t *testing.T) {
	f := newFixture(t, testDSN, true)
	svc := New(fakeQuerier{version: "160004"}, f.runner, f.st, testDSN, false) // egress guard on
	err := svc.Save(context.Background(), Input{
		Endpoint: f.dst.Endpoint, Bucket: f.dst.Bucket, Region: f.dst.Region,
		AccessKey: f.dst.AccessKey, SecretKey: testS3Secret, Prefix: "ops",
		Recipient: f.id.Recipient().String(), Preset: "daily", Retention: 14,
	})
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("Save to a loopback storage with the guard on: want netguard.ErrBlocked, got %v", err)
	}
	if strings.Contains(err.Error(), testS3Secret) || len(err.Error()) > 1200 {
		t.Fatalf("error text = %q", err)
	}
	if v, verr := f.st.LoadView(context.Background()); verr != nil || v.Bucket != "" {
		t.Fatalf("a refused save stored %+v, %v", v, verr)
	}
}

func TestRemoveLeftovers(t *testing.T) {
	f := newFixture(t, testDSN, false)
	f.configure(t, nil)
	ctx := context.Background()

	// No run in flight: the runner is asked for the dump label, and its
	// count comes back.
	f.runner.rm = func(context.Context) (int, error) { return 3, nil }
	if n, err := f.svc.RemoveLeftovers(ctx); n != 3 || err != nil {
		t.Fatalf("RemoveLeftovers = %d, %v; want 3, nil", n, err)
	}
	if got := f.runner.removeCalls(); len(got) != 1 || got[0] != docker.OneOffRoleLabel+"="+StateDumpLabelValue {
		t.Fatalf("remove calls = %v", got)
	}
	rmErr := errors.New("docker is slow")
	f.runner.rm = func(context.Context) (int, error) { return 1, rmErr }
	if n, err := f.svc.RemoveLeftovers(ctx); n != 1 || !errors.Is(err, rmErr) {
		t.Fatalf("RemoveLeftovers with a failing runner = %d, %v", n, err)
	}

	// A run holds the slot: its own container must not be swept.
	entered, gate := make(chan struct{}), make(chan struct{})
	f.runner.fn = func(_ context.Context, _ docker.OneOffSpec, out io.Writer) error {
		close(entered)
		<-gate
		_, err := out.Write([]byte(dumpDone))
		return err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- f.svc.Run(ctx, TriggerManual) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("run never reached the runner")
	}
	before := len(f.runner.removeCalls())
	if n, err := f.svc.RemoveLeftovers(ctx); n != 0 || err != nil {
		t.Errorf("RemoveLeftovers during a run = %d, %v; want 0, nil", n, err)
	}
	if got := len(f.runner.removeCalls()); got != before {
		t.Errorf("the runner was asked to remove containers while a run was in flight")
	}
	close(gate)
	select {
	case <-errCh: // the storage is unreachable; only the slot matters here
	case <-time.After(30 * time.Second):
		t.Fatal("run did not finish")
	}

	// The sweep holds the slot: a run started meanwhile is refused.
	f.runner.fn = nil
	sweeping, done := make(chan struct{}), make(chan struct{})
	f.runner.rm = func(context.Context) (int, error) {
		close(sweeping)
		<-done
		return 0, nil
	}
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		_, _ = f.svc.RemoveLeftovers(ctx)
	}()
	select {
	case <-sweeping:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep never reached the runner")
	}
	if err := f.svc.Start(TriggerManual); !errors.Is(err, ErrStateBackupRunning) {
		t.Errorf("Start during the sweep: %v; want ErrStateBackupRunning", err)
	}
	close(done)
	<-swept
	if n := f.svc.InFlight(); n != 0 {
		t.Errorf("InFlight after the sweep = %d", n)
	}
}

// The label value is per service: a run and the sweep use the same one, so
// a test can scope both to its own containers.
func TestDumpLabelValueIsPerService(t *testing.T) {
	f := newFixture(t, testDSN, false)
	f.configure(t, nil)
	f.svc.labelValue = "state-dump-test-x"
	_ = f.svc.Run(context.Background(), TriggerManual) // the storage is unreachable; only the spec matters
	if sp := f.runner.calls(); len(sp) != 1 || sp[0].Labels[docker.OneOffRoleLabel] != "state-dump-test-x" {
		t.Fatalf("specs = %+v", sp)
	}
	if _, err := f.svc.RemoveLeftovers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.runner.removeCalls(); len(got) != 1 || got[0] != docker.OneOffRoleLabel+"=state-dump-test-x" {
		t.Fatalf("remove calls = %v", got)
	}
}

func TestRemoveLeftoversNoRunner(t *testing.T) {
	svc := New(fakeQuerier{version: "160004"}, nil, NewDBStore(db.New(testutil.NewTestDB(t))), testDSN, true)
	if n, err := svc.RemoveLeftovers(context.Background()); n != 0 || err != nil {
		t.Fatalf("RemoveLeftovers without a runner = %d, %v", n, err)
	}
}
