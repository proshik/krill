package docker

import (
	"context"
	"errors"
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

// Every volume operation must reconnect for cleanup after its operation-owned
// transport closes. A detached HTTP context alone cannot revive that transport.
func TestVolumeSidecarsReconnectForCleanup(t *testing.T) {
	for _, operation := range []string{"archive", "restore", "chown"} {
		for _, mode := range []string{"worker", "missing-worker", "local"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				var mu sync.Mutex
				removed := false
				entered := make(chan struct{})
				peerDone := make(chan struct{})
				node := "worker"
				if mode == "local" {
					node = ""
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				handler := func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch {
					case strings.HasSuffix(r.URL.Path, "/info"):
						fmt.Fprint(w, `{"Swarm":{"NodeID":"worker"}}`)
					case strings.HasSuffix(r.URL.Path, "/images/json"):
						fmt.Fprint(w, `[{"Id":"image"}]`)
					case strings.HasSuffix(r.URL.Path, "/containers/create"):
						fmt.Fprint(w, `{"Id":"sidecar"}`)
					case strings.HasSuffix(r.URL.Path, "/attach"):
						c, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						defer c.Close()
						fmt.Fprint(c, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
						close(entered)
						<-ctx.Done()
						c.Close()
						close(peerDone)
					case strings.HasSuffix(r.URL.Path, "/start"):
						w.WriteHeader(http.StatusNoContent)
					case r.Method == http.MethodDelete:
						mu.Lock()
						removed = true
						mu.Unlock()
						w.WriteHeader(http.StatusNoContent)
					default:
						t.Errorf("unexpected worker %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}
				daemon := httptest.NewServer(http.HandlerFunc(handler))
				defer daemon.Close()
				manager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/info") {
						fmt.Fprint(w, `{"Swarm":{"NodeID":"manager"}}`)
						return
					}
					t.Errorf("worker cleanup reached manager: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}))
				defer manager.Close()
				newClient := func(url string) (*client.Client, error) {
					return client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(url, "http://")), client.WithVersion("1.52"))
				}
				localURL := manager.URL
				if mode == "local" {
					localURL = daemon.URL
				}
				local, err := newClient(localURL)
				if err != nil {
					t.Fatal(err)
				}
				defer local.Close()
				acquired, released := 0, 0
				e := &dockerEngine{cli: local, remoteProvider: func(ownerCtx context.Context, id string) (*client.Client, func() error, error) {
					if id == "" {
						return nil, nil, nil
					}
					if err := ownerCtx.Err(); err != nil {
						return nil, nil, err
					}
					acquired++
					if acquired > 1 {
						deadline, ok := ownerCtx.Deadline()
						if !ok || time.Until(deadline) > sidecarCleanupTimeout {
							t.Error("cleanup connection has no bounded deadline")
						}
					}
					if acquired > 1 && mode == "missing-worker" {
						return nil, nil, nil
					}
					// Emulate an SSH transport owned by the original operation, including its
					// raw HTTP upgrade connection. Once canceled it cannot dial even with a
					// new HTTP request context.
					remote, err := client.NewClientWithOpts(client.WithHost("tcp://worker"), client.WithVersion("1.52"), client.WithDialContext(func(dctx context.Context, _, _ string) (net.Conn, error) {
						if err := ownerCtx.Err(); err != nil {
							return nil, errors.New("SSH transport closed")
						}
						raw, err := (&net.Dialer{}).DialContext(dctx, "tcp", strings.TrimPrefix(daemon.URL, "http://"))
						if err != nil {
							return nil, err
						}
						return &cancelConn{Conn: raw, stop: context.AfterFunc(ownerCtx, func() { raw.Close() })}, nil
					}))
					if err != nil {
						return nil, nil, err
					}
					return remote, func() error { released++; return remote.Close() }, nil
				}}
				done := make(chan error, 1)
				go func() {
					switch operation {
					case "archive":
						done <- e.VolumeArchive(ctx, "data", io.Discard, node)
					case "restore":
						done <- e.VolumeRestore(ctx, "data", strings.NewReader(""), node)
					case "chown":
						done <- e.VolumeChown(ctx, "data", 1000, 1000, node)
					}
				}()
				select {
				case <-entered:
				case err := <-done:
					t.Fatalf("operation stopped before cancellation: %v", err)
				case <-time.After(3 * time.Second):
					t.Fatal("attach not reached")
				}
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("operation did not finish")
				}
				<-peerDone
				mu.Lock()
				defer mu.Unlock()
				if removed != (mode != "missing-worker") {
					t.Fatalf("cleanup on %s: removed=%v", mode, removed)
				}
				if mode != "local" {
					wantReleases := 2
					if mode == "missing-worker" {
						wantReleases = 1
					}
					if acquired != 2 || released != wantReleases {
						t.Fatalf("node clients: acquired=%d released=%d", acquired, released)
					}
				}
			})
		}
	}
}
