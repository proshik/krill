package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// fakeOneOffDaemon answers the calls RunOneOff makes; attach handles the
// hijacked attach request and removed reports whether the container was deleted.
type fakeOneOffDaemon struct {
	attach  func(w http.ResponseWriter)
	mu      sync.Mutex
	removed bool
}

func (d *fakeOneOffDaemon) engine(t *testing.T) *dockerEngine {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			fmt.Fprint(w, `[{"Id":"image"}]`)
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			fmt.Fprint(w, `{"Id":"oneoff"}`)
		case strings.HasSuffix(r.URL.Path, "/attach"):
			d.attach(w)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/wait"):
			fmt.Fprint(w, `{"StatusCode":0}`)
		case r.Method == http.MethodDelete:
			d.mu.Lock()
			d.removed = true
			d.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return &dockerEngine{cli: cli}
}

// A daemon that accepts the attach request but never answers the upgrade
// must not hold RunOneOff past its context.
func TestRunOneOffAttachHandshakeHonorsContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	d := &fakeOneOffDaemon{attach: func(w http.ResponseWriter) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		<-release // never send the 101 response
	}}
	e := d.engine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.RunOneOff(ctx, OneOffSpec{Image: "img", Cmd: []string{"x"}}, &bytes.Buffer{}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunOneOff succeeded with a stalled attach")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunOneOff still blocked in the attach handshake after its context ended")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.removed {
		t.Error("the container was not removed")
	}
}

func frame(stream byte, payload string) []byte {
	h := []byte{stream, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

func TestFrameReader(t *testing.T) {
	whole := append(frame(1, "hello "), frame(2, "warn")...)
	whole = append(whole, frame(1, "")...)
	whole = append(whole, frame(1, "world")...)
	for _, tc := range []struct {
		name    string
		in      []byte
		wantOut string
		wantErr bool
	}{
		{"complete frames", whole, "hello world", false},
		{"empty stream", nil, "", false},
		{"cut inside a payload", whole[:len(whole)-2], "", true},
		{"cut inside a header", append(frame(1, "hello"), 1, 0, 0), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			// iotest.OneByteReader exercises headers split across reads.
			_, err := stdcopy.StdCopy(&out, &errOut, &frameReader{r: iotest.OneByteReader(bytes.NewReader(tc.in))})
			if tc.wantErr {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
				}
				return
			}
			if err != nil || out.String() != tc.wantOut {
				t.Fatalf("out = %q, err = %v", out.String(), err)
			}
		})
	}
}

// An attach stream that ends inside a frame must fail the run even though
// the container itself exits 0.
func TestRunOneOffTruncatedFrameFails(t *testing.T) {
	d := &fakeOneOffDaemon{attach: func(w http.ResponseWriter) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		fmt.Fprint(c, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		f := frame(1, strings.Repeat("x", 100))
		c.Write(append(frame(1, "head "), f[:20]...))
	}}
	e := d.engine(t)
	var out bytes.Buffer
	err := e.RunOneOff(context.Background(), OneOffSpec{Image: "img", Cmd: []string{"x"}}, &out)
	if err == nil {
		t.Fatalf("RunOneOff succeeded on a stream cut mid-frame (out %q)", out.String())
	}
}
