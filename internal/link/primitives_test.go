package link

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestPairingCodeRoundTrip(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	c := PairingCode{Addr: "203.0.113.10:443", SNI: "link.example.com", CASHA256: strings.Repeat("ab", 32), Secret: secret}
	s := c.Encode()
	if !strings.HasPrefix(s, "krl1_") || strings.ContainsAny(s, "+/=") {
		t.Fatalf("code %q: want krl1_ + unpadded base64url", s)
	}
	got, err := DecodeCode("  " + s + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != c {
		t.Fatalf("decoded %+v, want %+v", got, c)
	}
	if SecretSHA256(secret) == secret || len(SecretSHA256(secret)) != 64 {
		t.Fatal("SecretSHA256 must be a hex digest, not the secret")
	}
}

func TestPairingCodeRejects(t *testing.T) {
	good := PairingCode{Addr: "relay.example.com:443", SNI: "link.example.com", CASHA256: strings.Repeat("ab", 32), Secret: strings.Repeat("s", 43)}
	mutate := func(f func(*PairingCode)) string { c := good; f(&c); return c.Encode() }
	for name, in := range map[string]string{
		"empty":         "",
		"no prefix":     strings.TrimPrefix(good.Encode(), "krl1_"),
		"other version": "krl2_" + strings.TrimPrefix(good.Encode(), "krl1_"),
		"not base64":    "krl1_###",
		"not json":      "krl1_bm90IGpzb24",
		"no port":       mutate(func(c *PairingCode) { c.Addr = "relay.example.com" }),
		"bad port":      mutate(func(c *PairingCode) { c.Addr = "relay.example.com:0" }),
		"no sni":        mutate(func(c *PairingCode) { c.SNI = "" }),
		"short ca":      mutate(func(c *PairingCode) { c.CASHA256 = "abcd" }),
		"short secret":  mutate(func(c *PairingCode) { c.Secret = "x" }),
		"too long":      "krl1_" + strings.Repeat("A", 4096),
	} {
		if _, err := DecodeCode(in); !errors.Is(err, ErrBadCode) {
			t.Errorf("%s: err = %v, want ErrBadCode", name, err)
		}
	}
}

func TestValidPeerName(t *testing.T) {
	for _, ok := range []string{"msk", "a", "msk-1", "0x"} {
		if !ValidPeerName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-msk", "msk-", "MSK", "msk.example", "msk 1", strings.Repeat("a", 33)} {
		if ValidPeerName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestIssuePeerFromCSR(t *testing.T) {
	now := time.Now()
	caPair, err := NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, csrPEM, err := NewCSR()
	if err != nil {
		t.Fatal(err)
	}
	req, err := parseCSR(csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, notAfter, err := issuePeer(caPair, req, "msk", now)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ParseCert(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "msk" {
		t.Errorf("CN %q, want msk", cert.Subject.CommonName)
	}
	if d := cert.NotAfter.Sub(now); d < PeerLifetime-time.Minute || d > PeerLifetime+time.Minute {
		t.Errorf("lifetime %v, want about a year", d)
	}
	if !notAfter.Equal(cert.NotAfter.UTC().Truncate(time.Second)) {
		t.Errorf("returned notAfter %v, cert says %v", notAfter, cert.NotAfter)
	}
	caCert, _ := ParseCert(caPair.CertPEM)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("client cert does not chain to the link CA: %v", err)
	}
	// The key the client keeps matches the certificate it got.
	k, err := parseKey(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := publicKeySHA256(&k.PublicKey)
	b, _ := publicKeySHA256(cert.PublicKey)
	if a != b {
		t.Fatal("issued certificate is not for the CSR's key")
	}
}

func TestNeedsRenewal(t *testing.T) {
	issued := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nb := issued.Add(-backdate)
	na := issued.Add(300 * 24 * time.Hour)
	if needsRenewal(nb, na, issued.Add(199*24*time.Hour)) {
		t.Error("before two thirds: no renewal")
	}
	if !needsRenewal(nb, na, issued.Add(200*24*time.Hour)) {
		t.Error("at two thirds: renew")
	}
}

func TestProxyHeaderRoundTrip(t *testing.T) {
	for _, tc := range []struct{ src, dst string }{
		{"203.0.113.7:51234", "10.0.0.2:8445"},
		{"[2001:db8::7]:51234", "[2001:db8::2]:8445"},
	} {
		src, dst := netip.MustParseAddrPort(tc.src), netip.MustParseAddrPort(tc.dst)
		rest := []byte("\x16\x03\x01after")
		r := bytes.NewReader(append(EncodeProxyHeader(src, dst), rest...))
		h, err := ReadProxyHeader(r)
		if err != nil {
			t.Fatal(err)
		}
		if h.Src != src || h.Dst != dst {
			t.Fatalf("got %v → %v, want %v → %v", h.Src, h.Dst, src, dst)
		}
		left, _ := io.ReadAll(r)
		if !bytes.Equal(left, rest) {
			t.Fatalf("header read past its end: left %q", left)
		}
	}
}

func TestProxyHeaderRejects(t *testing.T) {
	good := EncodeProxyHeader(netip.MustParseAddrPort("203.0.113.7:1"), netip.MustParseAddrPort("10.0.0.2:2"))
	edit := func(i int, b byte) []byte { c := append([]byte{}, good...); c[i] = b; return c }
	for name, in := range map[string][]byte{
		"tls first":     []byte("\x16\x03\x01\x02\x00\x01\x00\x01\xfc\x03\x03aaaaaaaaaa"),
		"v1 text":       []byte("PROXY TCP4 203.0.113.7 10.0.0.2 1 2\r\n"),
		"version 1":     edit(12, 0x11),
		"local command": edit(12, 0x20),
		"udp":           edit(13, 0x12),
		"unix":          edit(13, 0x31),
		"short ipv4":    append(edit(15, 4), good[16:20]...),
		"huge body":     append(edit(14, 0xff), make([]byte, 64)...),
	} {
		if _, err := ReadProxyHeader(bytes.NewReader(in)); !errors.Is(err, ErrNotProxy) {
			t.Errorf("%s: err = %v, want ErrNotProxy", name, err)
		}
	}
	for n := 0; n < len(good); n++ {
		if _, err := ReadProxyHeader(bytes.NewReader(good[:n])); !errors.Is(err, ErrNotProxy) {
			t.Errorf("truncated to %d bytes: err = %v, want ErrNotProxy", n, err)
		}
	}
}

func FuzzReadProxyHeader(f *testing.F) {
	f.Add(EncodeProxyHeader(netip.MustParseAddrPort("203.0.113.7:1"), netip.MustParseAddrPort("10.0.0.2:2")))
	f.Add(EncodeProxyHeader(netip.MustParseAddrPort("[2001:db8::1]:1"), netip.MustParseAddrPort("[2001:db8::2]:2")))
	f.Add([]byte("\r\n\r\n\x00\r\nQUIT\n\x21\x11\xff\xff"))
	f.Fuzz(func(t *testing.T, in []byte) {
		r := bytes.NewReader(in)
		h, err := ReadProxyHeader(r)
		if err != nil {
			if !errors.Is(err, ErrNotProxy) {
				t.Fatalf("error %v is not ErrNotProxy", err)
			}
			return
		}
		consumed := len(in) - r.Len()
		if consumed > 16+maxProxyBody {
			t.Fatalf("consumed %d bytes, more than a header can be", consumed)
		}
		if !h.Src.IsValid() || !h.Dst.IsValid() {
			t.Fatalf("accepted header with invalid addresses: %+v", h)
		}
	})
}

func TestStreamHeaderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHeader(&buf, StreamControl, map[string]any{"proto": 1}); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("rest")
	typ, body, err := ReadHeader(&buf)
	if err != nil || typ != StreamControl || string(body) != `{"proto":1}` {
		t.Fatalf("got %v %q %v", typ, body, err)
	}
	if buf.String() != "rest" {
		t.Fatalf("header read past its end: %q", buf.String())
	}
}

func TestStreamHeaderLimits(t *testing.T) {
	if err := WriteHeader(io.Discard, StreamControl, strings.Repeat("x", MaxHeader)); !errors.Is(err, ErrHeaderTooLarge) {
		t.Fatalf("writing an oversized header: %v", err)
	}
	// A header announcing MaxHeader+1 bytes is refused before its body.
	in := []byte{byte(StreamEgress), 0x10, 0x01}
	r := bytes.NewReader(append(in, make([]byte, MaxHeader+1)...))
	if _, _, err := ReadHeader(r); !errors.Is(err, ErrHeaderTooLarge) {
		t.Fatalf("err = %v, want ErrHeaderTooLarge", err)
	}
	if r.Len() != MaxHeader+1 {
		t.Fatalf("read %d body bytes of an oversized header", MaxHeader+1-r.Len())
	}
	for n := 0; n < 3+2; n++ {
		full := []byte{byte(StreamControl), 0, 2, '{', '}'}
		if _, _, err := ReadHeader(bytes.NewReader(full[:n])); err == nil {
			t.Errorf("truncated to %d bytes: no error", n)
		}
	}
}

// A peer that opens a stream and sends nothing is cut off after the timeout.
func TestStreamHeaderTimeout(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	start := time.Now()
	_, _, err := readHeaderWithin(a, 50*time.Millisecond)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout took far too long")
	}
}

// A relay certificate that fails the pinned check is a TLS failure on the
// page, not a network one.
func TestPinnedFailureIsTLS(t *testing.T) {
	now := time.Now()
	ca, _ := NewCA(now)
	srv, _ := IssueServer(ca, "link.example.com", now)
	chain, err := tlsCert(srv, ca.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := NewCA(now)
	otherFP, _ := CertFingerprint(other.CertPEM)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		_ = tls.Server(b, &tls.Config{Certificates: []tls.Certificate{chain}, MinVersion: tls.VersionTLS13}).Handshake()
	}()
	err = tls.Client(a, pinnedTLS("link.example.com", otherFP, nil)).Handshake()
	if err == nil {
		t.Fatal("a relay with another CA passed the pin")
	}
	if got := category(err); got != CatTLS {
		t.Fatalf("category %q, want tls (err %v)", got, err)
	}
}
