package link

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/cryptobyte"
)

// maxClientHello bounds the ClientHello a connection may send before Krill
// decides on it: post-quantum key shares make it a few kilobytes, never this
// much.
const maxClientHello = 64 << 10

// ErrNoSNI is a TLS ClientHello without a server name: an egress connection
// must name the host it wants.
var ErrNoSNI = errors.New("ClientHello carries no server name")

// ErrNotClientHello is a connection that does not open with a TLS ClientHello.
var ErrNotClientHello = errors.New("not a TLS ClientHello")

// ErrECH is a ClientHello with Encrypted Client Hello: the server name that
// counts is inside it, out of the link's sight, so it is refused.
var ErrECH = errors.New("ClientHello uses Encrypted Client Hello")

// extECH is the encrypted_client_hello extension (RFC 9849).
const extECH = 0xfe0d

// ReadClientHello reads the TLS records that carry a connection's ClientHello
// and returns them as read — to be written on unchanged — and the server name
// it asks for, lower-cased. It reads nothing past the ClientHello's last
// record. The connection is not trusted: every length is checked, and a
// ClientHello longer than maxClientHello is refused.
func ReadClientHello(r io.Reader) ([]byte, string, error) {
	var raw, hs []byte
	for {
		var hdr [5]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrNotClientHello, err)
		}
		// A handshake record (22) of TLS 1.0 to 1.3 framing (major 3).
		if hdr[0] != 0x16 || hdr[1] != 3 {
			return nil, "", ErrNotClientHello
		}
		n := int(hdr[3])<<8 | int(hdr[4])
		if n == 0 || n > 1<<14 || len(hs)+n > maxClientHello+4 {
			return nil, "", fmt.Errorf("%w: record of %d bytes", ErrNotClientHello, n)
		}
		rec := make([]byte, n)
		if _, err := io.ReadFull(r, rec); err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrNotClientHello, err)
		}
		raw = append(append(raw, hdr[:]...), rec...)
		hs = append(hs, rec...)
		if len(hs) < 4 {
			continue
		}
		if hs[0] != 1 { // client_hello
			return nil, "", ErrNotClientHello
		}
		want := 4 + (int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3]))
		if want > maxClientHello {
			return nil, "", fmt.Errorf("%w: %d-byte ClientHello", ErrNotClientHello, want)
		}
		if len(hs) < want {
			continue
		}
		if len(hs) > want {
			// Another handshake message in the same flight: a client
			// sends nothing after its ClientHello before the server
			// answers.
			return nil, "", fmt.Errorf("%w: data after the ClientHello", ErrNotClientHello)
		}
		sni, err := parseClientHelloSNI(hs[4:want])
		if err != nil {
			return nil, "", err
		}
		return raw, sni, nil
	}
}

// parseClientHelloSNI finds the server_name extension in a ClientHello body
// (RFC 8446 §4.1.2, RFC 6066 §3).
func parseClientHelloSNI(body []byte) (string, error) {
	s := cryptobyte.String(body)
	var (
		version    uint16
		random     []byte
		sessionID  cryptobyte.String
		suites     cryptobyte.String
		comp       cryptobyte.String
		extensions cryptobyte.String
	)
	if !s.ReadUint16(&version) || !s.ReadBytes(&random, 32) ||
		!s.ReadUint8LengthPrefixed(&sessionID) || !s.ReadUint16LengthPrefixed(&suites) ||
		!s.ReadUint8LengthPrefixed(&comp) {
		return "", fmt.Errorf("%w: malformed ClientHello", ErrNotClientHello)
	}
	if s.Empty() {
		return "", ErrNoSNI // no extensions at all
	}
	if !s.ReadUint16LengthPrefixed(&extensions) || !s.Empty() {
		return "", fmt.Errorf("%w: malformed extensions", ErrNotClientHello)
	}
	name, found := "", false
	for !extensions.Empty() {
		var typ uint16
		var data cryptobyte.String
		if !extensions.ReadUint16(&typ) || !extensions.ReadUint16LengthPrefixed(&data) {
			return "", fmt.Errorf("%w: malformed extension", ErrNotClientHello)
		}
		if typ == extECH {
			// The visible name would be the front end's public name; the
			// one the server acts on is encrypted.
			return "", ErrECH
		}
		if typ != 0 { // server_name
			continue
		}
		if found {
			return "", fmt.Errorf("%w: two server_name extensions", ErrNotClientHello)
		}
		found = true
		var list cryptobyte.String
		if !data.ReadUint16LengthPrefixed(&list) || !data.Empty() {
			return "", fmt.Errorf("%w: malformed server_name", ErrNotClientHello)
		}
		for !list.Empty() {
			var nameType uint8
			var host cryptobyte.String
			if !list.ReadUint8(&nameType) || !list.ReadUint16LengthPrefixed(&host) {
				return "", fmt.Errorf("%w: malformed server_name", ErrNotClientHello)
			}
			if nameType != 0 { // host_name
				continue
			}
			if name != "" {
				return "", fmt.Errorf("%w: two host names", ErrNotClientHello)
			}
			if !validSNI(host) {
				return "", fmt.Errorf("%w: invalid host name", ErrNotClientHello)
			}
			name = strings.ToLower(string(host))
		}
	}
	if name == "" {
		return "", ErrNoSNI
	}
	return name, nil
}

// validSNI accepts the bytes a DNS host name may carry: letters, digits,
// hyphens and dots, at most 253 of them, no trailing dot (RFC 6066).
func validSNI(b []byte) bool {
	if len(b) == 0 || len(b) > 253 || b[len(b)-1] == '.' {
		return false
	}
	for _, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}
