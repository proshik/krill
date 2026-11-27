package link

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CodePrefix starts every pairing code; the digit is the code format.
const CodePrefix = "krl1_"

// CodeLifetime is how long a pairing code can be redeemed.
const CodeLifetime = 10 * time.Minute

// maxCodeLen bounds what Decode reads: a code is pasted by a person.
const maxCodeLen = 2048

// PairingCode is what the relay operator hands to the client operator. Secret
// is a one-time credential: show it once, store only its SHA-256, never log it.
type PairingCode struct {
	Addr     string `json:"addr"`      // host:port the client dials
	SNI      string `json:"sni"`       // the relay's link host
	CASHA256 string `json:"ca_sha256"` // hex SHA-256 of the link CA certificate
	Secret   string `json:"secret"`
}

// ErrBadCode is any pairing code that does not decode into a usable one. It
// carries no detail on purpose: the input is a credential.
var ErrBadCode = errors.New("not a valid pairing code")

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Encode renders the code as krl1_<base64url(JSON)>.
func (c PairingCode) Encode() string {
	b, _ := json.Marshal(c) // a struct of strings always marshals
	return CodePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCode parses and validates a pasted pairing code.
func DecodeCode(s string) (PairingCode, error) {
	s = strings.TrimSpace(s)
	if len(s) > maxCodeLen || !strings.HasPrefix(s, CodePrefix) {
		return PairingCode{}, ErrBadCode
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, CodePrefix))
	if err != nil {
		return PairingCode{}, ErrBadCode
	}
	var c PairingCode
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return PairingCode{}, ErrBadCode
	}
	if err := ValidAddr(c.Addr); err != nil || c.SNI == "" || !hex64.MatchString(c.CASHA256) || len(c.Secret) < 32 {
		return PairingCode{}, ErrBadCode
	}
	return c, nil
}

// ValidAddr checks a host:port the client dials.
func ValidAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return errors.New("want host:port")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return errors.New("port must be 1..65535")
	}
	return nil
}

// NewSecret returns a random pairing secret.
func NewSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// SecretSHA256 is what the relay stores of a pairing secret.
func SecretSHA256(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

var peerName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidPeerName checks a peer name: a short lower-case label, as it appears in
// the certificate's CN, logs and the page.
func ValidPeerName(s string) bool { return peerName.MatchString(s) }
