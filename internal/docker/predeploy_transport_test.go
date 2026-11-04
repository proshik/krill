package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// Exercise both ambiguous create and the SDK's raw HTTP upgrade, which stops
// using the request context after dialing. The fake daemon owns real sockets.
func TestPredeployPartialCreateAndAttachCancellation(t *testing.T) {
	for _, mode := range []string{"lost-create", "stranger-app", "stranger-deployment", "stranger-label", "blackhole-attach"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			created, removed := false, false
			entered := make(chan struct{})
			peerClosed := make(chan struct{})
			var stuck net.Conn
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/nodes"):
					fmt.Fprint(w, `[{"ID":"manager","Spec":{"Role":"manager","Availability":"active"},"Status":{"State":"ready"}}]`)
				case strings.HasSuffix(r.URL.Path, "/info"):
					fmt.Fprint(w, `{"Swarm":{"NodeID":"manager"}}`)
				case strings.Contains(r.URL.Path, "/images/"):
					fmt.Fprint(w, `{"Id":"sha256:abc"}`)
				case strings.HasSuffix(r.URL.Path, "/containers/create"):
					mu.Lock()
					created = true
					mu.Unlock()
					if mode == "blackhole-attach" {
						fmt.Fprint(w, `{"Id":"owned-id"}`)
					} else {
						fmt.Fprint(w, `{"Id":`)
					}
				case strings.HasSuffix(r.URL.Path, "/attach"):
					c, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					stuck = c
					mu.Unlock()
					close(entered)
					io.Copy(io.Discard, c)
					c.Close()
					close(peerClosed)
				case r.Method == "GET" && strings.Contains(r.URL.Path, "/containers/"):
					app, deployment, label := "17", "23", "1"
					if mode == "stranger-app" {
						app = "999"
					}
					if mode == "stranger-deployment" {
						deployment = "999"
					}
					if mode == "stranger-label" {
						label = "0"
					}
					fmt.Fprintf(w, `{"Id":"owned-id","Config":{"Labels":{"krill.predeploy":%q,"krill.predeploy.app":%q,"krill.predeploy.deployment":%q}}}`, label, app, deployment)
				case r.Method == "DELETE":
					if !strings.HasSuffix(r.URL.Path, "/containers/owned-id") {
						t.Errorf("delete must use inspected ID: %s", r.URL.Path)
					}
					mu.Lock()
					removed = true
					mu.Unlock()
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.52"))
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			acquired, released := 0, 0
			e := &dockerEngine{cli: cli, remoteProvider: func(providerCtx context.Context, _ string) (*client.Client, func() error, error) {
				if err := providerCtx.Err(); err != nil {
					return nil, nil, err
				}
				acquired++
				remote, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.52"))
				if err != nil {
					return nil, nil, err
				}
				return remote, func() error { released++; return remote.Close() }, nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- e.RunPredeploy(ctx, PredeploySpec{AppID: 17, DeploymentID: 23, Image: "sha256:" + strings.Repeat("a", 64)}, io.Discard)
			}()
			if mode == "blackhole-attach" {
				select {
				case <-entered:
				case err := <-done:
					t.Fatalf("before attach: %v", err)
				case <-time.After(2 * time.Second):
					t.Fatal("attach never reached")
				}
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("failure succeeded")
				}
			case <-time.After(time.Second):
				mu.Lock()
				if stuck != nil {
					stuck.Close()
				}
				mu.Unlock()
				<-done
				t.Error("operation ignored cancellation")
			}
			mu.Lock()
			defer mu.Unlock()
			if acquired != 2 || released != 2 {
				t.Errorf("node client lifecycle: acquired=%d released=%d", acquired, released)
			}
			if !created || removed != (!strings.HasPrefix(mode, "stranger-")) {
				t.Errorf("created=%v removed=%v", created, removed)
			}
			if mode == "blackhole-attach" {
				select {
				case <-peerClosed:
				case <-time.After(time.Second):
					t.Error("attach socket leaked")
				}
			}
		})
	}
}
