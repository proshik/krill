package docker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

func TestVerifiedNodeRejectsMissingWorkerRegistration(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/v1.52/info" {
			t.Errorf("unexpected Docker mutation: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Swarm":{"NodeID":"manager-id"}}`))
	}))
	defer srv.Close()
	cli, err := client.NewClientWithOpts(client.WithHost(srv.URL), client.WithVersion("1.52"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	e := &dockerEngine{cli: cli, remoteProvider: func(context.Context, string) (*client.Client, func() error, error) { return nil, nil, nil }}
	if _, release, err := e.verifiedNodeClient(t.Context(), "unregistered-worker"); err == nil {
		release()
		t.Fatal("missing registration silently selected manager")
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	if got, release, err := e.verifiedNodeClient(t.Context(), "manager-id"); err != nil || got != cli {
		t.Fatalf("local manager rejected: %v", err)
	} else {
		release()
	}
}

func TestReleaseRejectsPlacementLabelDriftBeforeMutation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/nodes") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"ID":"selected","Spec":{"Availability":"active","Labels":{}},"Status":{"State":"ready"}}]`))
	}))
	defer srv.Close()
	cli, err := client.NewClientWithOpts(client.WithHost(srv.URL), client.WithVersion("1.52"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	e := &dockerEngine{cli: cli}
	err = e.ValidateRelease(t.Context(), ServiceSpec{Image: "sha256:" + strings.Repeat("a", 64), Constraints: []string{"node.labels.krill.place.1==1"}}, []string{"selected"})
	if err == nil || !strings.Contains(err.Error(), "labels changed") {
		t.Fatalf("drift accepted: %v", err)
	}
}

func TestReleaseNetworkIsValidatedOnManager(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			workerNetworkCalls := 0
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/info"):
					fmt.Fprint(w, `{"Swarm":{"NodeID":"worker"}}`)
				case strings.Contains(r.URL.Path, "/networks/"):
					workerNetworkCalls++
					http.NotFound(w, r)
				case strings.Contains(r.URL.Path, "/images/"):
					fmt.Fprint(w, `{"Id":"sha256:abc"}`)
				default:
					t.Errorf("unexpected worker request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer worker.Close()
			wc, _ := client.NewClientWithOpts(client.WithHost(worker.URL), client.WithVersion("1.52"))
			defer wc.Close()
			manager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/nodes"):
					fmt.Fprint(w, `[{"ID":"worker","Spec":{"Availability":"active","Role":"worker"},"Status":{"State":"ready"}}]`)
				case strings.Contains(r.URL.Path, "/networks/"):
					if missing {
						http.NotFound(w, r)
					} else {
						fmt.Fprint(w, `{"Name":"org-network","Driver":"overlay","Scope":"swarm"}`)
					}
				default:
					t.Errorf("unexpected manager request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer manager.Close()
			mc, _ := client.NewClientWithOpts(client.WithHost(manager.URL), client.WithVersion("1.52"))
			defer mc.Close()
			e := &dockerEngine{cli: mc, remoteProvider: func(context.Context, string) (*client.Client, func() error, error) {
				return wc, func() error { return nil }, nil
			}}
			err := e.ValidateRelease(t.Context(), ServiceSpec{Image: "sha256:" + strings.Repeat("a", 64), Network: "org-network"}, nil)
			if (err != nil) != missing {
				t.Fatalf("missing=%v: %v", missing, err)
			}
			if workerNetworkCalls != 0 {
				t.Fatal("inspected worker-local overlay")
			}
		})
	}
}
