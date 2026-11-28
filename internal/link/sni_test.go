package link

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// clientHello captures the first flight a Go TLS client sends for sni.
func clientHello(t testing.TB, sni string) []byte {
	t.Helper()
	c, s := net.Pipe()
	go func() {
		tc := tls.Client(c, &tls.Config{ServerName: sni, InsecureSkipVerify: true}) //nolint:gosec // never completes
		_ = tc.Handshake()
	}()
	_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
	var buf bytes.Buffer
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(s, hdr); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(hdr[3])<<8|int(hdr[4]))
	if _, err := io.ReadFull(s, body); err != nil {
		t.Fatal(err)
	}
	buf.Write(hdr)
	buf.Write(body)
	_ = s.Close()
	_ = c.Close()
	return buf.Bytes()
}

// splitRecords re-frames one handshake record as two.
func splitRecords(rec []byte, at int) []byte {
	body := rec[5:]
	out := []byte{0x16, rec[1], rec[2], byte(at >> 8), byte(at)}
	out = append(out, body[:at]...)
	rest := len(body) - at
	out = append(out, 0x16, rec[1], rec[2], byte(rest>>8), byte(rest))
	return append(out, body[at:]...)
}

func TestReadClientHello(t *testing.T) {
	hello := clientHello(t, "API.Example.com")
	after := []byte("application data that must stay unread")

	raw, sni, err := ReadClientHello(bytes.NewReader(append(append([]byte{}, hello...), after...)))
	if err != nil || sni != "api.example.com" || !bytes.Equal(raw, hello) {
		t.Fatalf("ReadClientHello = (%d bytes, %q, %v), want the record and api.example.com", len(raw), sni, err)
	}

	split := splitRecords(hello, 10)
	r := bytes.NewReader(append(append([]byte{}, split...), after...))
	raw, sni, err = ReadClientHello(r)
	if err != nil || sni != "api.example.com" || !bytes.Equal(raw, split) {
		t.Fatalf("two records: (%d bytes, %q, %v)", len(raw), sni, err)
	}
	if rest, _ := io.ReadAll(r); !bytes.Equal(rest, after) {
		t.Fatalf("read past the ClientHello: %q left", rest)
	}
}

func TestReadClientHello_Refusals(t *testing.T) {
	hello := clientHello(t, "api.example.com")
	noSNI := clientHello(t, "")
	tooLong := append([]byte{0x16, 3, 1, 0x40, 0x01}, make([]byte, 1<<14+1)...)
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"no server name", noSNI, ErrNoSNI},
		{"plain HTTP", []byte("GET / HTTP/1.1\r\nHost: api.example.com\r\n\r\n"), ErrNotClientHello},
		{"truncated record", hello[:len(hello)-1], ErrNotClientHello},
		{"truncated header", hello[:3], ErrNotClientHello},
		{"empty", nil, ErrNotClientHello},
		{"oversized record", tooLong, ErrNotClientHello},
		{"not a client_hello", append([]byte{0x16, 3, 1, 0, 4}, 2, 0, 0, 0), ErrNotClientHello},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, sni, err := ReadClientHello(bytes.NewReader(tc.in))
			if !errors.Is(err, tc.want) || sni != "" {
				t.Fatalf("ReadClientHello = (%q, %v), want %v", sni, err, tc.want)
			}
		})
	}
}

// A ClientHello whose declared length is far beyond the limit is refused
// before the rest of it is read.
func TestReadClientHello_DeclaredTooLong(t *testing.T) {
	in := []byte{0x16, 3, 1, 0, 4, 1, 0x10, 0, 0}
	if _, _, err := ReadClientHello(bytes.NewReader(in)); !errors.Is(err, ErrNotClientHello) {
		t.Fatalf("err = %v", err)
	}
}

func FuzzReadClientHello(f *testing.F) {
	f.Add(clientHello(f, "api.example.com"))
	f.Add(clientHello(f, ""))
	f.Add(splitRecords(clientHello(f, "a.example.org"), 7))
	f.Add([]byte{0x16, 3, 1, 0, 4, 1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, in []byte) {
		raw, sni, err := ReadClientHello(bytes.NewReader(in))
		if err != nil {
			if raw != nil || sni != "" {
				t.Fatalf("error with output: %d bytes, %q", len(raw), sni)
			}
			return
		}
		if !bytes.HasPrefix(in, raw) || sni == "" || !validSNI([]byte(sni)) {
			t.Fatalf("raw not a prefix of the input or bad sni %q", sni)
		}
	})
}

// hello builds a ClientHello record with the given extensions (type, body).
func helloWith(exts ...[]byte) []byte {
	body := []byte{3, 3}
	body = append(body, make([]byte, 32)...) // random
	body = append(body, 0)                   // session id
	body = append(body, 0, 2, 0x13, 0x01)    // one cipher suite
	body = append(body, 1, 0)                // null compression
	var ext []byte
	for _, e := range exts {
		ext = append(ext, e...)
	}
	body = append(body, byte(len(ext)>>8), byte(len(ext)))
	body = append(body, ext...)
	hs := append([]byte{1, 0, byte(len(body) >> 8), byte(len(body))}, body...)
	return append([]byte{0x16, 3, 1, byte(len(hs) >> 8), byte(len(hs))}, hs...)
}

func sniExt(host string) []byte {
	name := append([]byte{0, byte(len(host) >> 8), byte(len(host))}, host...)
	list := append([]byte{byte(len(name) >> 8), byte(len(name))}, name...)
	return append([]byte{0, 0, byte(len(list) >> 8), byte(len(list))}, list...)
}

// A ClientHello with Encrypted Client Hello is refused: the name the server
// acts on is the encrypted inner one, so the visible one proves nothing.
func TestReadClientHello_RefusesECH(t *testing.T) {
	if _, sni, err := ReadClientHello(bytes.NewReader(helloWith(sniExt("api.example.com")))); err != nil || sni != "api.example.com" {
		t.Fatalf("plain hello: %q %v", sni, err)
	}
	ech := []byte{0xfe, 0x0d, 0, 1, 0}
	if _, _, err := ReadClientHello(bytes.NewReader(helloWith(sniExt("api.example.com"), ech))); !errors.Is(err, ErrECH) {
		t.Fatalf("ECH hello: %v, want ErrECH", err)
	}
}
