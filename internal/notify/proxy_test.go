package notify

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeBotAPI stands in for api.telegram.org: a TLS server whose certificate
// the proxied client trusts under the name example.com.
func fakeBotAPI(t *testing.T) (addr string, paths *[]string) {
	t.Helper()
	loopbackOnly(t)
	var mu sync.Mutex
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Path)
		mu.Unlock()
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	guard := proxyTestTransport
	proxyTestTransport = func(tr *http.Transport) {
		guard(tr)
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: "example.com"}
	}
	return srv.Listener.Addr().String(), &got
}

// loopbackOnly makes the proxied client refuse to dial anything but a
// loopback address (the test proxies): were the proxy bypassed, the
// client would dial api.telegram.org and fail here, offline.
func loopbackOnly(t *testing.T) {
	t.Helper()
	proxyTestTransport = func(tr *http.Transport) {
		var d net.Dialer
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
				return nil, errors.New("test: refused to dial " + addr + " (the proxy was bypassed)")
			}
			return d.DialContext(ctx, network, addr)
		}
	}
	t.Cleanup(func() { proxyTestTransport = nil })
}

// recorder keeps what a test proxy was asked to connect to.
type recorder struct {
	mu      sync.Mutex
	targets []string
}

func (r *recorder) add(s string) { r.mu.Lock(); r.targets = append(r.targets, s); r.mu.Unlock() }
func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.targets...)
}

func pipe(a, b net.Conn) {
	go func() { io.Copy(a, b); a.Close() }()
	io.Copy(b, a)
	b.Close()
}

// socks5Proxy is a minimal SOCKS5 server (RFC 1928/1929) that sends every
// CONNECT to backend and records the target it was asked for. user/pass
// empty: no authentication.
func socks5Proxy(t *testing.T, backend, user, pass string) (string, *recorder) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	rec := &recorder{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				hdr := make([]byte, 2)
				if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 5 {
					return
				}
				io.ReadFull(br, make([]byte, hdr[1]))
				if user == "" {
					c.Write([]byte{5, 0})
				} else {
					c.Write([]byte{5, 2})
					v := make([]byte, 2)
					io.ReadFull(br, v)
					u := make([]byte, v[1])
					io.ReadFull(br, u)
					l := make([]byte, 1)
					io.ReadFull(br, l)
					p := make([]byte, l[0])
					io.ReadFull(br, p)
					if string(u) != user || string(p) != pass {
						c.Write([]byte{1, 1})
						rec.add("auth-failed")
						return
					}
					c.Write([]byte{1, 0})
				}
				req := make([]byte, 4)
				if _, err := io.ReadFull(br, req); err != nil {
					return
				}
				var host string
				switch req[3] {
				case 3: // a name: the proxy resolves it
					l := make([]byte, 1)
					io.ReadFull(br, l)
					name := make([]byte, l[0])
					io.ReadFull(br, name)
					host = string(name)
				case 1:
					ip := make([]byte, 4)
					io.ReadFull(br, ip)
					host = "ip:" + net.IP(ip).String()
				default:
					return
				}
				port := make([]byte, 2)
				io.ReadFull(br, port)
				rec.add(net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))))
				up, err := net.Dial("tcp", backend)
				if err != nil {
					c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				pipe(&bufConn{Conn: c, r: br}, up)
			}(c)
		}
	}()
	return ln.Addr().String(), rec
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// connectProxy is an HTTP proxy that tunnels CONNECT to backend.
func connectProxy(t *testing.T, backend, user, pass string) (string, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			rec.add("not-connect " + r.Host)
			http.Error(w, "connect only", http.StatusMethodNotAllowed)
			return
		}
		if user != "" {
			want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
			if r.Header.Get("Proxy-Authorization") != want {
				rec.add("auth-failed")
				w.WriteHeader(http.StatusProxyAuthRequired)
				return
			}
		}
		rec.add(r.Host)
		up, err := net.Dial("tcp", backend)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		pipe(c, up)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), rec
}

const fakeToken = "123456:TEST-not-a-real-token"

func TestTelegramGoesThroughTheProxy(t *testing.T) {
	for _, c := range []struct {
		name, scheme, userinfo string
		start                  func(t *testing.T, backend, user, pass string) (string, *recorder)
	}{
		{"socks5h", "socks5h", "", socks5Proxy},
		{"socks5 with credentials", "socks5", "krill:s3cret-pw@", socks5Proxy},
		{"http CONNECT with credentials", "http", "krill:s3cret-pw@", connectProxy},
	} {
		t.Run(c.name, func(t *testing.T) {
			direct := stubDirect(t) // a regression must not reach the real Bot API
			backend, paths := fakeBotAPI(t)
			user, pass := "", ""
			if c.userinfo != "" {
				user, pass = "krill", "s3cret-pw"
			}
			addr, rec := c.start(t, backend, user, pass)
			svc := New(&fakeStore{})
			via, err := svc.SetTelegramProxy(c.scheme + "://" + c.userinfo + addr)
			if err != nil {
				t.Fatal(err)
			}
			if want := c.scheme + "://" + addr; via != want {
				t.Errorf("label %q, want %q (no credentials)", via, want)
			}
			if err := svc.SendTest(context.Background(), fakeToken, "42", "hi"); err != nil {
				t.Fatal(err)
			}
			// The name, not an address: api.telegram.org is resolved by the
			// proxy (it may not resolve, or resolve badly, where Krill runs).
			if got := rec.list(); len(got) != 1 || got[0] != "api.telegram.org:443" {
				t.Errorf("proxy was asked for %v, want api.telegram.org:443", got)
			}
			if len(*paths) != 1 || (*paths)[0] != "/bot"+fakeToken+"/sendMessage" {
				t.Errorf("bot API got %v", *paths)
			}
			if len(*direct) != 0 {
				t.Errorf("also sent directly: %v", *direct)
			}
		})
	}
}

// roundTripFunc records requests the direct client would make, so no test
// ever reaches the real api.telegram.org.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func stubDirect(t *testing.T) *[]string {
	t.Helper()
	var hosts []string
	prev := httpClient.Transport
	httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: http.Header{}}, nil
	})
	t.Cleanup(func() { httpClient.Transport = prev })
	return &hosts
}

func TestTelegramWithoutProxyIsDirect(t *testing.T) {
	direct := stubDirect(t)
	svc := New(&fakeStore{})
	if via, err := svc.SetTelegramProxy("  "); via != "" || err != nil {
		t.Fatalf("empty setting: %q %v", via, err)
	}
	if err := svc.SendTest(context.Background(), fakeToken, "42", "hi"); err != nil {
		t.Fatal(err)
	}
	if len(*direct) != 1 || (*direct)[0] != "api.telegram.org" {
		t.Fatalf("direct sends: %v", *direct)
	}
}

// A wrong value fails every send with the reason; it never sends directly
// and never repeats the value (it may hold a password).
func TestTelegramInvalidProxyNeverSendsDirect(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1:1080",
		"ftp://127.0.0.1:1080",
		"socks4://127.0.0.1:1080",
		"socks5h://",
		"socks5h://127.0.0.1:1080/path",
		"socks5h://krill:s3cret-pw@127.0.0.1:port",
		"http://krill:s3cret-pw@[::1",
		"krill:s3cret-pw@127.0.0.1:1080", // no scheme: "krill" parses as one
	} {
		t.Run(raw, func(t *testing.T) {
			direct := stubDirect(t)
			svc := New(&fakeStore{})
			via, err := svc.SetTelegramProxy(raw)
			if err == nil || via != "" {
				t.Fatalf("accepted: %q", via)
			}
			if !strings.Contains(err.Error(), ProxyEnv) || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "krill") {
				t.Errorf("startup error %q: want the setting's name and no credentials", err)
			}
			serr := svc.SendTest(context.Background(), fakeToken, "42", "hi")
			if serr == nil || !strings.Contains(serr.Error(), ProxyEnv) || strings.Contains(serr.Error(), "s3cret") {
				t.Errorf("send error %v", serr)
			}
			if len(*direct) != 0 {
				t.Errorf("sent directly to %v", *direct)
			}
		})
	}
}

func TestTelegramProxyErrorHidesCredentials(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listens: the proxy is down
	loopbackOnly(t)
	for _, scheme := range []string{"socks5h", "http"} {
		svc := New(&fakeStore{})
		if _, err := svc.SetTelegramProxy(scheme + "://krill:s3cret-pw@" + addr); err != nil {
			t.Fatal(err)
		}
		err := svc.SendTest(context.Background(), fakeToken, "42", "hi")
		if err == nil || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), fakeToken) ||
			!strings.Contains(err.Error(), addr) || strings.Contains(err.Error(), "bypassed") {
			t.Errorf("%s: %v (want a failure to reach the proxy at %s, without credentials)", scheme, err, addr)
		}
	}
}

// The proxy is the Telegram sender's alone: a request through
// http.DefaultClient (the shared transport most of Krill uses) still goes
// direct — it would reach the proxy had the setting been applied to the
// shared transport, since http.ProxyURL proxies loopback too.
func TestTelegramProxyIsNotGlobal(t *testing.T) {
	backend, _ := fakeBotAPI(t)
	paddr, rec := socks5Proxy(t, backend, "", "")
	svc := New(&fakeStore{})
	if _, err := svc.SetTelegramProxy("socks5h://" + paddr); err != nil {
		t.Fatal(err)
	}
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer other.Close()
	resp, err := http.Get(other.URL) // http.DefaultClient, like most of Krill
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !hit || len(rec.list()) != 0 {
		t.Fatalf("a default client went through the Telegram proxy: hit=%v proxy=%v", hit, rec.list())
	}
	if httpClient.Transport != nil {
		t.Fatal("the direct Telegram client was changed")
	}
}
