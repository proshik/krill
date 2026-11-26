package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/proshik/krill/internal/krillcli/client"
	"github.com/proshik/krill/internal/krillcli/deployflow"
)

// runCLI runs one invocation against h the way Execute does — argv in, exit
// code out — with the token and server from the environment (no stored
// context, no krill.yaml) and stdout/stderr captured.
func runCLI(t *testing.T, h http.HandlerFunc, args ...string) (code int, stdout, stderr string, requests int) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Chdir(t.TempDir())
	t.Setenv("KRILL_CLI_HOME", t.TempDir())
	t.Setenv("KRILL_TOKEN", "krill_pat_test0000000000000000000000000000000000")
	t.Setenv("KRILL_SERVER", srv.URL)
	t.Setenv("KRILL_CLI_CONTEXT", "")

	outF, errF := tempFile(t), tempFile(t)
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outF, errF
	root := newRoot()
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	os.Stdout, os.Stderr = oldOut, oldErr
	code = exitCodeFor(classify(err))
	if err != nil {
		stderr = classifyHint(err).Error() + "\n"
	}
	return code, readBack(t, outF), readBack(t, errF) + stderr, int(n.Load())
}

func tempFile(t *testing.T) *os.File {
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func readBack(t *testing.T, f *os.File) string {
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return string(b)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// asCurrent answers the compatibility probe as a current server would and
// hands every other request to h.
func asCurrent(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/whoami" {
			writeJSON(w, 200, currentWhoami)
			return
		}
		h(w, r)
	}
}

const currentWhoami = `{"user_id":1,"org_id":3,"org_name":"acme","level":"write","role":"admin","can_write":true,
	"instance":{"url":"https://krill.example.com","version":"v0.14.0"}}`

func TestLogsWaitForExitCodes(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"seen", `{"found":true,"stopped_by":"match","lines":[{"t":"2026-11-26T10:00:00Z","lvl":"info","msg":"listening"}],"following_since":"2026-11-26T09:59:59Z"}`, deployflow.ExitOK},
		{"timed out", `{"found":false,"stopped_by":"timeout","lines":[],"following_since":"2026-11-26T09:59:59Z"}`, deployflow.ExitTimeout},
		{"log ended", `{"found":false,"stopped_by":"ended","lines":[]}`, deployflow.ExitTimeout},
		{"broken read", `{"found":false,"stopped_by":"error","lines":[],"incomplete":true}`, deployflow.ExitTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut, _ := runCLI(t, asCurrent(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("wait_for") != "listening" || r.URL.Query().Get("timeout") != "30" {
					t.Errorf("query = %v", r.URL.Query())
				}
				writeJSON(w, 200, tc.body)
			}), "logs", "acme/production/bot", "--wait-for", "listening", "--timeout", "30s")
			if code != tc.want {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.want, out, errOut)
			}
			if tc.want != 0 && !strings.Contains(errOut, "--since and --contains") {
				t.Fatalf("a missed wait should point at the history read: %s", errOut)
			}
		})
	}
}

func TestLogsWaitForJSONIsTheServersAnswer(t *testing.T) {
	code, out, _, _ := runCLI(t, asCurrent(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, `{"found":false,"stopped_by":"timeout","lines":[],"following_since":"2026-11-26T09:59:59Z","incomplete":true}`)
	}), "--json", "logs", "17", "--wait-for", "x")
	if code != deployflow.ExitTimeout {
		t.Fatalf("exit %d", code)
	}
	var res client.LogWaitResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("stdout is not the JSON answer: %v\n%s", err, out)
	}
	if res.StoppedBy != "timeout" || !res.Incomplete || res.FollowingSince == "" {
		t.Fatalf("answer = %+v", res)
	}
}

// TestLogsFlagConflictsAreRefusedUnsent mirrors the server's own rules, so the
// mistake costs no request and gets a message about flags, not parameters.
func TestLogsFlagConflictsAreRefusedUnsent(t *testing.T) {
	for _, args := range [][]string{
		{"logs", "17", "--wait-for", "x", "--contains", "y"},
		{"logs", "17", "--wait-for", "x", "--since", "10m"},
		{"logs", "17", "--wait-for", "x", "--level", "error"},
		{"logs", "17", "--timeout", "10s"},
		{"logs", "17", "--wait-for", "x", "--timeout", "5m"},
	} {
		code, _, _, n := runCLI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `[]`) }, args...)
		if code != deployflow.ExitUsage || n != 0 {
			t.Fatalf("%v: exit %d after %d requests, want 2 and none", args, code, n)
		}
	}
}

func TestLogsWaitForOnAnOldServer(t *testing.T) {
	code, _, errOut, _ := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/whoami" {
			writeJSON(w, 200, `{"org_name":"acme","level":"read","role":"member"}`)
			return
		}
		writeJSON(w, 500, `{"code":"internal","message":"internal error"}`)
	}, "logs", "17", "--wait-for", "listening")
	if code != deployflow.ExitUsage || !strings.Contains(errOut, "older than") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestConfigOnAnOldServerSaysUpdate(t *testing.T) {
	code, _, errOut, _ := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 400, `{"code":"invalid","message":"app \"acme/production/bot/config\" is neither a numeric id nor a project/environment/app path"}`)
	}, "config", "acme/production/bot")
	if code != deployflow.ExitUsage || !strings.Contains(errOut, "Update Krill") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestBackupsOnAnOldServerSaysUpdate(t *testing.T) {
	code, _, errOut, _ := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/whoami" {
			writeJSON(w, 200, `{"org_name":"acme"}`)
			return
		}
		http.NotFound(w, r)
	}, "backups")
	if code != deployflow.ExitUsage || !strings.Contains(errOut, "Update Krill") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if strings.Contains(errOut, "KRILL_AGENT_API_ENABLED") {
		t.Fatalf("an older server was explained as a switched-off API: %s", errOut)
	}
}

func TestBackupsFiltersContradict(t *testing.T) {
	code, _, _, n := runCLI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `[]`) },
		"backups", "--app", "17", "--env", "acme/production")
	if code != deployflow.ExitUsage || n != 0 {
		t.Fatalf("exit %d after %d requests", code, n)
	}
}

func TestBackupRunReadTokenIsAuth(t *testing.T) {
	code, _, _, _ := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 403, `{"code":"forbidden","message":"this token is read-only"}`)
	}, "backups", "run", "database", "3")
	if code != deployflow.ExitAuth {
		t.Fatalf("exit %d, want %d", code, deployflow.ExitAuth)
	}
}

func TestWhoamiShowsTheInstance(t *testing.T) {
	code, out, _, _ := runCLI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, currentWhoami) }, "whoami")
	if code != 0 || !strings.Contains(out, "https://krill.example.com  v0.14.0") || !strings.Contains(out, "can write: yes") {
		t.Fatalf("exit %d:\n%s", code, out)
	}

	code, out, _, _ = runCLI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, currentWhoami) }, "--json", "whoami")
	var who client.Whoami
	if code != 0 || json.Unmarshal([]byte(out), &who) != nil || who.InstanceVersion() != "v0.14.0" || who.Instance.URL == "" {
		t.Fatalf("exit %d, json %q", code, out)
	}

	_, out, _, _ = runCLI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `{"org_name":"acme","level":"read"}`) }, "whoami")
	if !strings.Contains(out, "older than") {
		t.Fatalf("an old server's missing instance should say so:\n%s", out)
	}
}

func TestStatusShowsTheDiagnostics(t *testing.T) {
	code, out, _, _ := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/deployments/") {
			writeJSON(w, 200, `{"id":418,"status":"running","settling":true}`)
			return
		}
		writeJSON(w, 200, `{"id":17,"path":"acme/production/bot","source_type":"image","status":"deploying","replicas":"0/1",
			"last_deploy_id":418,"last_deploy_status":"running","failed_since_deploy":2,"failed_since":"2026-11-26T10:00:00Z",
			"tasks":[{"id":"abcdef0123456789","node":"node-1","state":"failed","desired_state":"shutdown","error":"task: non-zero exit (137)","exit_code":137}],
			"health":[{"task_id":"abcdef0123456789","status":"unhealthy","failing_streak":3,"last_exit_code":1}],
			"usage":[{"task_id":"abcdef0123456789","mem_bytes":536870912,"mem_limit_bytes":536870912,"cpu_pct":12.5}]}`)
	}, "status", "17")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"exit 137", "non-zero exit (137)", "unhealthy", "failing streak 3", "512 MiB / 512 MiB", "cpu 12.5%", "failed     2", "Swarm is still converging"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status is missing %q:\n%s", want, out)
		}
	}
}

func TestDeploymentWatchJSON(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   int
	}{{"done", 0}, {"error", deployflow.ExitFailed}} {
		code, out, _, _ := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("wait") != "120" {
				t.Errorf("--json watch should wait 120 s on the server, query %q", r.URL.RawQuery)
			}
			writeJSON(w, 200, `{"id":9,"status":"`+tc.status+`","trigger":"api","log_tail":"step 1\n"}`)
		}, "--json", "deployment", "9", "--watch")
		var d client.DeploymentDetail
		if code != tc.want || json.Unmarshal([]byte(out), &d) != nil || d.Status != tc.status {
			t.Fatalf("%s: exit %d, stdout %q", tc.status, code, out)
		}
	}
}

func TestClassifyOldServerAndUsage(t *testing.T) {
	for _, err := range []error{&client.OldServerError{Op: "x"}, &client.UsageError{Err: errors.New("bad ref")}} {
		if got := deployflow.CodeOf(classify(err)); got != deployflow.ExitUsage {
			t.Fatalf("%T: code %d", err, got)
		}
	}
}

// TestAwaitBackupVerdicts: a finished entry is this run's result only when its
// last run is the one started.
func TestAwaitBackupVerdicts(t *testing.T) {
	const at = "2026-11-26T10:00:00.123456Z"
	started := client.BackupStarted{Kind: "database", ID: 3, Status: "started", StartedAt: at}
	tests := []struct {
		name  string
		entry client.BackupInfo
		want  int
	}{
		{"this run succeeded", client.BackupInfo{LastRunAt: at, LastStatus: "ok"}, deployflow.ExitOK},
		{"this run failed", client.BackupInfo{LastRunAt: at, LastStatus: "error", LastError: "destination unreachable"}, deployflow.ExitFailed},
		{"recorded nothing", client.BackupInfo{LastRunAt: "2026-11-25T03:00:00.000000Z", LastStatus: "ok"}, deployflow.ExitFailed},
		{"never ran", client.BackupInfo{}, deployflow.ExitFailed},
		{"a later run overwrote it", client.BackupInfo{LastRunAt: "2026-11-26T10:05:00.000000Z", LastStatus: "ok"}, deployflow.ExitTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			entry := tc.entry
			entry.Kind, entry.ID = "database", 3
			w := backupWaiter{
				List: func(context.Context) ([]client.BackupInfo, error) {
					calls++
					if calls == 1 {
						return []client.BackupInfo{{Kind: "database", ID: 3, Running: true}, {Kind: "volume", ID: 3}}, nil
					}
					return []client.BackupInfo{entry}, nil
				},
				Sleep: func(context.Context, time.Duration) error { return nil },
				Now:   time.Now,
			}
			_, err := awaitBackup(context.Background(), w, started, time.Minute)
			if got := deployflow.CodeOf(err); got != tc.want {
				t.Fatalf("code %d, want %d (%v)", got, tc.want, err)
			}
			if calls != 2 {
				t.Fatalf("listed %d times; a running entry must be waited out", calls)
			}
		})
	}
}

func TestAwaitBackupTimesOut(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	w := backupWaiter{
		List: func(context.Context) ([]client.BackupInfo, error) {
			return []client.BackupInfo{{Kind: "volume", ID: 4, Running: true}}, nil
		},
		Sleep: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil },
		Now:   func() time.Time { return now },
	}
	_, err := awaitBackup(context.Background(), w, client.BackupStarted{Kind: "volume", ID: 4, StartedAt: "2026-11-26T10:00:00Z"}, 2*time.Minute)
	if deployflow.CodeOf(err) != deployflow.ExitTimeout {
		t.Fatalf("want ExitTimeout, got %v", err)
	}
}

// TestEmptyWaitForIsRefused: `--wait-for "$READY_LINE"` with the variable
// unset must not become an ordinary read that exits 0.
func TestEmptyWaitForIsRefused(t *testing.T) {
	for _, v := range []string{"", "  "} {
		code, _, _, n := runCLI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `[]`) },
			"logs", "17", "--wait-for", v)
		if code != deployflow.ExitUsage || n != 0 {
			t.Fatalf("--wait-for %q: exit %d after %d requests, want 2 and none", v, code, n)
		}
	}
}

// TestBackupRunRefusesABadTimeoutBeforeStarting: the run applies retention,
// so a mistyped --timeout must not start one.
func TestBackupRunRefusesABadTimeoutBeforeStarting(t *testing.T) {
	for _, d := range []string{"0s", "-1s"} {
		code, _, _, n := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, `{"kind":"database","id":3,"status":"started","started_at":"2026-11-26T10:00:00.000000Z"}`)
		}, "backups", "run", "database", "3", "--wait", "--timeout", d)
		if code != deployflow.ExitUsage || n != 0 {
			t.Fatalf("--timeout %s: exit %d after %d requests, want 2 and none", d, code, n)
		}
	}
}

// TestLogWaitWithNoAnswerIsUnknown: a held log wait that gets no answer (a
// proxy giving up with 504, a dropped link) says nothing about the line.
func TestLogWaitWithNoAnswerIsUnknown(t *testing.T) {
	code, _, errOut, _ := runCLI(t, asCurrent(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
	}), "logs", "17", "--wait-for", "listening", "--timeout", "5s")
	if code != deployflow.ExitTimeout || !strings.Contains(errOut, "unknown") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestWhoamiHidesCredentialsInTheServerURL(t *testing.T) {
	if got := displayURL("https://user:s3cret@krill.example.com/base?x=1#f"); got != "https://krill.example.com/base" {
		t.Fatalf("displayURL = %q", got)
	}
}

func TestWhoamiJSONOnAnOldServerHasNoInstance(t *testing.T) {
	code, out, _, _ := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, `{"org_name":"acme","level":"read","role":"member"}`)
	}, "--json", "whoami")
	if code != 0 || strings.Contains(out, "instance") {
		t.Fatalf("exit %d, json %s", code, out)
	}
}

// TestAwaitBackupStalledListIsCutAtTheDeadline: a list request that never
// answers must not carry --wait past --timeout.
func TestAwaitBackupStalledListIsCutAtTheDeadline(t *testing.T) {
	w := backupWaiter{
		List: func(ctx context.Context) ([]client.BackupInfo, error) {
			<-ctx.Done()
			return nil, errors.New("cannot reach https://k: " + ctx.Err().Error())
		},
		Sleep: sleepCtx,
		Now:   time.Now,
	}
	start := time.Now()
	_, err := awaitBackup(context.Background(), w, client.BackupStarted{Kind: "volume", ID: 4, StartedAt: "2026-11-26T10:00:00Z"}, 100*time.Millisecond)
	if deployflow.CodeOf(err) != deployflow.ExitTimeout || strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("want a timeout, got %v", err)
	}
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("waited %s on a 100ms timeout", took)
	}
}

func TestEmptyBackupFilterIsRefused(t *testing.T) {
	for _, args := range [][]string{{"backups", "--app", ""}, {"backups", "--env", " "}} {
		code, _, _, n := runCLI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `[]`) }, args...)
		if code != deployflow.ExitUsage || n != 0 {
			t.Fatalf("%v: exit %d after %d requests, want 2 and none", args, code, n)
		}
	}
}

// TestBackupStartWithNoAnswerIsUnknown: the server starts the run before it
// replies, so a lost reply means "it may be running" (4), not "failed" (1).
func TestBackupStartWithNoAnswerIsUnknown(t *testing.T) {
	code, _, errOut, _ := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}, "backups", "run", "database", "3", "--wait")
	if code != deployflow.ExitTimeout || !strings.Contains(errOut, "may have started") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

// TestAwaitBackupEdgeCases: this run's recorded result wins even when a
// restore made the entry busy again; an entry deleted mid-run is unknown.
func TestAwaitBackupEdgeCases(t *testing.T) {
	const at = "2026-11-26T10:00:00.123456Z"
	started := client.BackupStarted{Kind: "database", ID: 3, StartedAt: at}
	run := func(entries ...[]client.BackupInfo) error {
		i := 0
		w := backupWaiter{
			List: func(context.Context) ([]client.BackupInfo, error) {
				e := entries[min(i, len(entries)-1)]
				i++
				return e, nil
			},
			Sleep: func(context.Context, time.Duration) error { return nil },
			Now:   time.Now,
		}
		_, err := awaitBackup(context.Background(), w, started, time.Minute)
		return err
	}
	busy := client.BackupInfo{Kind: "database", ID: 3, Running: true}
	if err := run([]client.BackupInfo{busy}, []client.BackupInfo{{Kind: "database", ID: 3, Running: true, LastRunAt: at, LastStatus: "ok"}}); err != nil {
		t.Fatalf("a recorded ok under a restore's busy flag: %v", err)
	}
	if err := run([]client.BackupInfo{busy}, nil); deployflow.CodeOf(err) != deployflow.ExitTimeout {
		t.Fatalf("a deleted entry: want 4, got %v", err)
	}
}

func TestBackupWaitJSONKeepsOtherRunsApart(t *testing.T) {
	st := client.BackupStarted{Kind: "volume", ID: 4, StartedAt: "2026-11-26T10:00:00.000001Z"}
	mine := backupWaitJSON(st, client.BackupInfo{Kind: "volume", ID: 4, LastRunAt: st.StartedAt, LastStatus: "ok"})
	if mine.Result == nil || mine.LastSeen != nil {
		t.Fatalf("this run's entry: %+v", mine)
	}
	other := backupWaitJSON(st, client.BackupInfo{Kind: "volume", ID: 4, LastRunAt: "2026-11-26T09:00:00Z", LastStatus: "ok"})
	if other.Result != nil || other.LastSeen == nil {
		t.Fatalf("another run's entry was presented as this one's: %+v", other)
	}
}
