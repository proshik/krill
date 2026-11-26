package deployflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/krillcli/client"
	"github.com/proshik/krill/internal/krillcli/deployflow"
)

// fakeClock is the time a test flow sees. Sleeping advances it, and so does
// a server that holds its answer.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// waitServer plays GET /deployments/{id}?wait=. holds says whether it knows
// the parameter: a current server holds the answer while the deployment runs
// (advancing the clock by the wait, or to the end if that comes first), an
// old one answers at once whatever was asked.
type waitServer struct {
	clock    *fakeClock
	holds    bool
	doneAt   time.Time
	settling bool

	asked []time.Duration // the wait of every request
	at    []time.Time     // when every request was sent
}

func (s *waitServer) Whoami(context.Context) (client.Whoami, error) { return client.Whoami{}, nil }
func (s *waitServer) AppStatus(context.Context, string) (client.AppStatus, error) {
	return client.AppStatus{}, nil
}
func (s *waitServer) Deployments(context.Context, string, int) ([]client.Deployment, error) {
	return nil, nil
}
func (s *waitServer) Deploy(context.Context, string, string) (client.Accepted, error) {
	return client.Accepted{}, nil
}

func (s *waitServer) DeploymentWait(_ context.Context, id int64, wait time.Duration) (client.DeploymentDetail, error) {
	s.asked = append(s.asked, wait)
	s.at = append(s.at, s.clock.t)
	// Every answer takes a moment on the wire, so a client that never pauses
	// still reaches its deadline (and fails the test) instead of looping on
	// a clock that never moves.
	s.clock.t = s.clock.t.Add(50 * time.Millisecond)
	if s.holds && s.clock.t.Before(s.doneAt) {
		end := s.clock.t.Add(wait)
		if end.After(s.doneAt) {
			end = s.doneAt
		}
		s.clock.t = end
	}
	d := client.DeploymentDetail{Deployment: client.Deployment{ID: id, Status: "running"}, Settling: s.settling}
	if !s.clock.t.Before(s.doneAt) {
		d.Status, d.Settling = "done", false
	}
	return d, nil
}

func waitFlow(s *waitServer, out *strings.Builder) (*deployflow.Flow, *[]time.Duration) {
	var sleeps []time.Duration
	return &deployflow.Flow{
		API: s, Out: out,
		Now: s.clock.now,
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			s.clock.t = s.clock.t.Add(d)
			return nil
		},
	}, &sleeps
}

// TestOldServerAnsweringAtOnceIsNotPolledHard is the compatibility promise: a
// Krill that predates ?wait= answers every request at once with "running".
// Asking again immediately would spend the token's 60 requests a minute in
// seconds, so a quick "running" must be followed by the ordinary poll pause.
func TestOldServerAnsweringAtOnceIsNotPolledHard(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	srv := &waitServer{clock: clock, doneAt: clock.t.Add(5 * time.Minute)}
	var out strings.Builder
	f, sleeps := waitFlow(srv, &out)

	if _, err := f.Follow(context.Background(), 7, 10*time.Minute, false); err != nil {
		t.Fatalf("follow: %v", err)
	}
	for i := 1; i < len(srv.at); i++ {
		if gap := srv.at[i].Sub(srv.at[i-1]); gap < 500*time.Millisecond {
			t.Fatalf("request %d came %s after the previous one; an old server must not be asked again at once", i, gap)
		}
	}
	// The same budget the old poll schedule kept: a five-minute deploy in
	// about forty requests, never more than the old schedule's first minute.
	if n := len(srv.at); n > 45 {
		t.Fatalf("%d requests for a five-minute deploy against an old server", n)
	}
	perMinute := map[int64]int{}
	for _, at := range srv.at {
		perMinute[at.Unix()/60]++
	}
	for minute, n := range perMinute {
		if n > 20 {
			t.Fatalf("%d requests in minute %d", n, minute)
		}
	}
	if len(*sleeps) == 0 {
		t.Fatal("no pause was taken between requests")
	}
}

// TestCurrentServerHoldsTheAnswer: a server that waits is asked again right
// away — the waiting already happened there — and two minutes per request
// when no log is streamed.
func TestCurrentServerHoldsTheAnswer(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	srv := &waitServer{clock: clock, holds: true, doneAt: clock.t.Add(5 * time.Minute)}
	var out strings.Builder
	f, sleeps := waitFlow(srv, &out)

	d, err := f.Follow(context.Background(), 7, 10*time.Minute, false)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if d.Status != "done" {
		t.Fatalf("final status = %q, want done", d.Status)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("paused %v between held requests; the server already waited", *sleeps)
	}
	if len(srv.asked) != 3 {
		t.Fatalf("%d requests for five minutes, want 3 (120 s + 120 s + the end): %v", len(srv.asked), srv.asked)
	}
	for _, w := range srv.asked {
		if w != 120*time.Second {
			t.Fatalf("asked to wait %s, want 120s: %v", w, srv.asked)
		}
	}
}

// TestStreamingWaitsTheFullWindowToo: the server saves the build log only
// when a deployment ends, so a terminal showing the log gains nothing from
// shorter requests — it asks for the full two minutes like everyone else.
func TestStreamingWaitsTheFullWindowToo(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	srv := &waitServer{clock: clock, holds: true, doneAt: clock.t.Add(3 * time.Minute)}
	var out strings.Builder
	f, _ := waitFlow(srv, &out)

	if _, err := f.Follow(context.Background(), 7, 10*time.Minute, true); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if len(srv.asked) != 2 || srv.asked[0] != 120*time.Second {
		t.Fatalf("asked %v, want two requests of up to 120s", srv.asked)
	}
}

// TestWaitNeverOutlastsTheTimeout: the last request asks only for what is
// left, so --timeout 50s is not a 120-second wait.
func TestWaitNeverOutlastsTheTimeout(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	srv := &waitServer{clock: clock, holds: true, doneAt: clock.t.Add(time.Hour)}
	var out strings.Builder
	f, _ := waitFlow(srv, &out)

	_, err := f.Follow(context.Background(), 7, 50*time.Second, false)
	if deployflow.CodeOf(err) != deployflow.ExitTimeout {
		t.Fatalf("want ExitTimeout, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if len(srv.asked) != 1 || srv.asked[0] != 50*time.Second {
		t.Fatalf("asked %v, want one 50s wait", srv.asked)
	}
}

// TestSettlingIsStillRunning: settling means applied, Swarm still converging.
// It is said once, never ends the wait, and names itself when the wait runs
// out — "still running" alone would send someone to the build log.
func TestSettlingIsStillRunning(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	srv := &waitServer{clock: clock, holds: true, doneAt: clock.t.Add(time.Hour), settling: true}
	var out strings.Builder
	f, _ := waitFlow(srv, &out)

	_, err := f.Follow(context.Background(), 7, 5*time.Minute, true)
	if deployflow.CodeOf(err) != deployflow.ExitTimeout {
		t.Fatalf("want ExitTimeout, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "Swarm is still converging") {
		t.Fatalf("the timeout should say the deployment is applied and converging: %v", err)
	}
	if n := strings.Count(out.String(), "Swarm is still converging"); n != 1 {
		t.Fatalf("the settling note was printed %d times:\n%s", n, out.String())
	}
}

// stallingAPI never answers: its request blocks until the context ends.
type stallingAPI struct{ waitServer }

func (s *stallingAPI) DeploymentWait(ctx context.Context, _ int64, _ time.Duration) (client.DeploymentDetail, error) {
	<-ctx.Done()
	return client.DeploymentDetail{}, errors.New("cannot reach https://k: " + ctx.Err().Error())
}

// TestStalledRequestIsCutAtTheDeadline: a server that never answers must
// not carry the watch past --timeout by the HTTP client's own slack.
func TestStalledRequestIsCutAtTheDeadline(t *testing.T) {
	f := &deployflow.Flow{API: &stallingAPI{}, Sleep: func(context.Context, time.Duration) error { return nil }}
	start := time.Now()
	_, err := f.Follow(context.Background(), 7, 100*time.Millisecond, false)
	if deployflow.CodeOf(err) != deployflow.ExitTimeout {
		t.Fatalf("want ExitTimeout, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if took := time.Since(start); took > 100*time.Millisecond+6*time.Second {
		t.Fatalf("the watch ran %s past a 100ms timeout", took)
	}
	if strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("the watch's own deadline was reported as an interrupt: %v", err)
	}
}

// TestRetryAfterIsCappedByTheDeadline: a Retry-After longer than what is left
// of the watch must not be slept in full.
func TestRetryAfterIsCappedByTheDeadline(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	var slept []time.Duration
	api := &throttledServer{waitServer: waitServer{clock: clock, doneAt: clock.t.Add(time.Hour)}}
	f := &deployflow.Flow{API: api, Now: clock.now, Sleep: func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		clock.t = clock.t.Add(d)
		return nil
	}}
	_, err := f.Follow(context.Background(), 7, 10*time.Second, false)
	if deployflow.CodeOf(err) != deployflow.ExitTimeout {
		t.Fatalf("want ExitTimeout, got %v", err)
	}
	for _, d := range slept {
		if d > 10*time.Second {
			t.Fatalf("slept %s inside a 10s watch", d)
		}
	}
}

type throttledServer struct{ waitServer }

func (s *throttledServer) DeploymentWait(context.Context, int64, time.Duration) (client.DeploymentDetail, error) {
	return client.DeploymentDetail{}, &client.APIError{HTTPStatus: 429, Code: "rate_limited", RetryAfter: time.Minute}
}

// TestProxyTimeoutIsLostContactNotFailure: a proxy in front of Krill that gives
// up on a held request answers 504. That says nothing about the deployment, so
// it is retried and, if it persists, unknown (4) — never "failed" (1).
func TestProxyTimeoutIsLostContactNotFailure(t *testing.T) {
	api := okAPI()
	api.statusErrAlways = &client.APIError{HTTPStatus: 504, Code: "http_504", Message: "Gateway Timeout"}
	f, _ := newFlow(api, &fakeDocker{outValue: "sha256:abc"})
	_, err := f.Follow(context.Background(), 7, time.Minute, false)
	if deployflow.CodeOf(err) != deployflow.ExitTimeout {
		t.Fatalf("want ExitTimeout, got %d (%v)", deployflow.CodeOf(err), err)
	}
	if api.statusCall < 2 {
		t.Fatalf("a 504 was not retried (%d calls)", api.statusCall)
	}
}
