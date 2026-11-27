package link

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// StreamType is the first byte of every stream.
type StreamType byte

const (
	StreamControl StreamType = 1 // client → relay, one per session
	StreamEgress  StreamType = 2 // client → relay (KRL-002)
	StreamIngress StreamType = 3 // relay → client (KRL-005)
	StreamPair    StreamType = 4 // client → relay, without a client certificate
)

func (t StreamType) String() string {
	switch t {
	case StreamControl:
		return "control"
	case StreamEgress:
		return "egress"
	case StreamIngress:
		return "ingress"
	case StreamPair:
		return "pair"
	}
	return fmt.Sprintf("unknown(%d)", byte(t))
}

// Stream header limits.
const (
	// MaxHeader bounds a header's JSON.
	MaxHeader = 4096
	// HeaderTimeout bounds how long a peer may take to send a header after
	// opening a stream.
	HeaderTimeout = 5 * time.Second
)

// ErrHeaderTooLarge is a header announcing more than MaxHeader bytes.
var ErrHeaderTooLarge = errors.New("stream header too large")

// WriteHeader writes a stream header: one type byte, a 2-byte big-endian
// length and that many bytes of JSON.
func WriteHeader(w io.Writer, t StreamType, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > MaxHeader {
		return ErrHeaderTooLarge
	}
	buf := make([]byte, 0, 3+len(body))
	buf = append(buf, byte(t))
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(body)))
	buf = append(buf, body...)
	_, err = w.Write(buf)
	return err
}

// ReadHeader reads a stream header. The length is checked before any of the
// body is read, so an oversized header costs three bytes.
func ReadHeader(r io.Reader) (StreamType, []byte, error) {
	var fixed [3]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint16(fixed[1:]))
	if n > MaxHeader {
		return 0, nil, ErrHeaderTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return StreamType(fixed[0]), body, nil
}

// readHeaderWithin is ReadHeader bounded by HeaderTimeout; the deadline is
// cleared afterwards, since a stream may stay idle for minutes once open.
func readHeaderWithin(c net.Conn, timeout time.Duration) (StreamType, []byte, error) {
	if err := c.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, nil, err
	}
	t, body, err := ReadHeader(c)
	if err != nil {
		return 0, nil, err
	}
	return t, body, c.SetReadDeadline(time.Time{})
}
