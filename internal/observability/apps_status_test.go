package observability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func fixtureExec(t *testing.T, down bool) ExecFunc {
	t.Helper()
	return func(_ context.Context, service string, cmd []string, out io.Writer) error {
		if service != AppsServiceName || len(cmd) != 5 || cmd[0] != "bash" || cmd[1] != "-c" || cmd[2] != statusScript || cmd[3] != "krill-status" {
			t.Fatalf("unexpected exec: %s %v", service, cmd)
		}
		name := "alloy_module_component.json"
		switch {
		case strings.Contains(cmd[4], "prometheus.scrape."):
			name = "alloy_scrape_component_up.json"
			if down {
				name = "alloy_scrape_component_down.json"
			}
		case strings.Contains(cmd[4], "discovery.dns."):
			name = "alloy_dns_component.json"
		case strings.Contains(cmd[4], "discovery.relabel."):
			name = "alloy_relabel_component.json"
		}
		body, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		_, err = fmt.Fprintf(out, "HTTP/1.0 200 OK\r\n\r\n%s", body)
		return err
	}
}
func TestReadAppsStatusFixtures(t *testing.T) {
	for _, down := range []bool{false, true} {
		health, eps, err := ReadAppsStatus(context.Background(), fixtureExec(t, down), 7, []int64{3})
		if err != nil {
			t.Fatal(err)
		}
		if !health.Healthy || len(eps) != 1 || !eps[0].Found || eps[0].Resolved != 1 || eps[0].Kept != 1 || len(eps[0].Targets) != 1 {
			t.Fatalf("health=%+v endpoints=%+v", health, eps)
		}
		target := eps[0].Targets[0]
		want := "up"
		if down {
			want = "down"
		}
		if target.Health != want || target.Node != "krill-cp-msk" || target.LastScrape.IsZero() {
			t.Fatalf("target=%+v", target)
		}
		if down && target.LastError != "server returned HTTP status 401 Unauthorized" {
			t.Fatalf("error=%q", target.LastError)
		}
	}
}
func TestReadAppsStatusFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		err        error
	}{
		{"exec", "", errors.New("exec unavailable")}, {"garbage", "garbage", nil}, {"json", "HTTP/1.0 200 OK\r\n\r\n{}", nil}, {"large", "HTTP/1.0 200 OK\r\n\r\n" + strings.Repeat("x", statusMaxBody), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ReadAppsStatus(context.Background(), func(_ context.Context, _ string, _ []string, out io.Writer) error {
				if tc.err != nil {
					return tc.err
				}
				_, err := io.WriteString(out, tc.body)
				return err
			}, 7, []int64{3})
			if err == nil {
				t.Fatal("expected failure")
			}
		})
	}
}
func TestReadAppsStatusMissingAndUnhealthy(t *testing.T) {
	real := fixtureExec(t, false)
	calls := 0
	h, eps, err := ReadAppsStatus(context.Background(), func(ctx context.Context, svc string, cmd []string, out io.Writer) error {
		calls++
		if strings.Contains(cmd[4], "prometheus.scrape") {
			_, err := io.WriteString(out, "HTTP/1.0 404 Not Found\r\n\r\n")
			return err
		}
		return real(ctx, svc, cmd, out)
	}, 7, []int64{3})
	if err != nil || !h.Healthy || len(eps) != 1 || eps[0].Found || calls != 2 {
		t.Fatalf("missing: %+v %+v %v calls=%d", h, eps, err, calls)
	}
	h, _, err = ReadAppsStatus(context.Background(), func(_ context.Context, _ string, _ []string, out io.Writer) error {
		_, err := io.WriteString(out, `HTTP/1.0 200 OK`+"\r\n\r\n"+`{"health":{"state":"unhealthy","message":"bad rules"}}`)
		return err
	}, 7, nil)
	if err != nil || h.Healthy || h.Message != "bad rules" {
		t.Fatalf("unhealthy: %+v %v", h, err)
	}
	if got := shortStatusText(strings.Repeat("x", 400)); len(got) != 300 {
		t.Fatal("unbounded error")
	}
}
