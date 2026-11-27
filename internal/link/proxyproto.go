package link

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
)

// proxySignature opens every PROXY protocol v2 header.
var proxySignature = []byte("\r\n\r\n\x00\r\nQUIT\n")

// maxProxyBody bounds the address block and TLVs a header may carry. The
// gateway sends addresses only; anything far longer is not the gateway.
const maxProxyBody = 512

// ProxyHeader is the part of a PROXY v2 header the link uses.
type ProxyHeader struct {
	Src netip.AddrPort // the connection's original source
	Dst netip.AddrPort // the address it was sent to
}

// ErrNotProxy is a connection that does not start with a usable PROXY v2
// header.
var ErrNotProxy = errors.New("no PROXY v2 header")

// ReadProxyHeader reads a PROXY protocol v2 header from r and nothing more.
// Only the PROXY command over TCP on IPv4 or IPv6 is accepted: the listener
// needs the source address, which LOCAL, UDP and UNIX headers do not give.
//
// The header is not authenticated: anything that reaches the port can send
// one. The caller accepts it only together with the gateway's client
// certificate on the same connection.
func ReadProxyHeader(r io.Reader) (ProxyHeader, error) {
	var fixed [16]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return ProxyHeader{}, fmt.Errorf("%w: %v", ErrNotProxy, err)
	}
	if !bytes.Equal(fixed[:12], proxySignature) {
		return ProxyHeader{}, fmt.Errorf("%w: bad signature", ErrNotProxy)
	}
	if fixed[12]>>4 != 2 {
		return ProxyHeader{}, fmt.Errorf("%w: version %d", ErrNotProxy, fixed[12]>>4)
	}
	if fixed[12]&0x0f != 1 {
		return ProxyHeader{}, fmt.Errorf("%w: not the PROXY command", ErrNotProxy)
	}
	n := int(binary.BigEndian.Uint16(fixed[14:16]))
	if n > maxProxyBody {
		return ProxyHeader{}, fmt.Errorf("%w: %d-byte body", ErrNotProxy, n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return ProxyHeader{}, fmt.Errorf("%w: %v", ErrNotProxy, err)
	}
	family, proto := fixed[13]>>4, fixed[13]&0x0f
	if proto != 1 {
		return ProxyHeader{}, fmt.Errorf("%w: not TCP", ErrNotProxy)
	}
	var h ProxyHeader
	switch family {
	case 1: // AF_INET
		if n < 12 {
			return ProxyHeader{}, fmt.Errorf("%w: short IPv4 block", ErrNotProxy)
		}
		src := netip.AddrFrom4([4]byte(body[0:4]))
		dst := netip.AddrFrom4([4]byte(body[4:8]))
		h.Src = netip.AddrPortFrom(src, binary.BigEndian.Uint16(body[8:10]))
		h.Dst = netip.AddrPortFrom(dst, binary.BigEndian.Uint16(body[10:12]))
	case 2: // AF_INET6
		if n < 36 {
			return ProxyHeader{}, fmt.Errorf("%w: short IPv6 block", ErrNotProxy)
		}
		src := netip.AddrFrom16([16]byte(body[0:16])).Unmap()
		dst := netip.AddrFrom16([16]byte(body[16:32])).Unmap()
		h.Src = netip.AddrPortFrom(src, binary.BigEndian.Uint16(body[32:34]))
		h.Dst = netip.AddrPortFrom(dst, binary.BigEndian.Uint16(body[34:36]))
	default:
		return ProxyHeader{}, fmt.Errorf("%w: address family %d", ErrNotProxy, family)
	}
	return h, nil
}

// EncodeProxyHeader writes a PROXY v2 header for src → dst (tests, and the
// sidecar path of later stages).
func EncodeProxyHeader(src, dst netip.AddrPort) []byte {
	out := append([]byte{}, proxySignature...)
	if src.Addr().Is4() && dst.Addr().Is4() {
		out = append(out, 0x21, 0x11, 0, 12)
		s, d := src.Addr().As4(), dst.Addr().As4()
		out = append(out, s[:]...)
		out = append(out, d[:]...)
	} else {
		out = append(out, 0x21, 0x21, 0, 36)
		s, d := src.Addr().As16(), dst.Addr().As16()
		out = append(out, s[:]...)
		out = append(out, d[:]...)
	}
	out = binary.BigEndian.AppendUint16(out, src.Port())
	out = binary.BigEndian.AppendUint16(out, dst.Port())
	return out
}
