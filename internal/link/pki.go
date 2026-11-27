package link

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Certificate lifetimes.
const (
	caLifetime     = 10 * 365 * 24 * time.Hour
	serverLifetime = 2 * 365 * 24 * time.Hour
	// localLifetime covers the self-signed gateway and listener certificates,
	// which never leave this installation and are pinned by fingerprint.
	localLifetime = 20 * 365 * 24 * time.Hour
	// backdate absorbs clock skew between the two installations.
	backdate = time.Hour
)

// PeerLifetime is how long a client certificate issued at pairing or renewal
// is valid; the client renews it at two thirds of that. A variable for tests.
var PeerLifetime = 365 * 24 * time.Hour

// ListenerServerName is the name the gateway verifies the link listener's
// certificate against. It never appears in DNS.
const ListenerServerName = "krill-link-listener"

// KeyPair is a PEM certificate and its PEM private key. Key is secret: store
// it only through secret.Enc and never log it.
type KeyPair struct {
	CertPEM string
	KeyPEM  string
}

func newKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func encodeKey(k *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

func encodeCert(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// ParseCert reads one PEM certificate.
func ParseCert(certPEM string) (*x509.Certificate, error) {
	b, _ := pem.Decode([]byte(certPEM))
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

// parseKey reads one PEM EC private key.
func parseKey(keyPEM string) (*ecdsa.PrivateKey, error) {
	b, _ := pem.Decode([]byte(keyPEM))
	if b == nil || b.Type != "EC PRIVATE KEY" {
		return nil, errors.New("not a PEM EC private key")
	}
	return x509.ParseECPrivateKey(b.Bytes)
}

// Fingerprint is the hex SHA-256 of a certificate's DER bytes.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// CertFingerprint is Fingerprint of a PEM certificate.
func CertFingerprint(certPEM string) (string, error) {
	c, err := ParseCert(certPEM)
	if err != nil {
		return "", err
	}
	return Fingerprint(c.Raw), nil
}

// publicKeySHA256 is the hex SHA-256 of a public key's PKIX encoding: what a
// redeemed pairing code remembers of the CSR, so that a retry with the same
// key gets the same certificate.
func publicKeySHA256(pub any) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// NewCA creates the relay's private link CA (ECDSA P-256).
func NewCA(now time.Time) (KeyPair, error) {
	k, err := newKey()
	if err != nil {
		return KeyPair{}, err
	}
	sn, err := serial()
	if err != nil {
		return KeyPair{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "krill link CA"},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return KeyPair{}, err
	}
	kp, err := encodeKey(k)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{CertPEM: encodeCert(der), KeyPEM: kp}, nil
}

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func loadCA(p KeyPair) (ca, error) {
	c, err := ParseCert(p.CertPEM)
	if err != nil {
		return ca{}, fmt.Errorf("ca cert: %w", err)
	}
	k, err := parseKey(p.KeyPEM)
	if err != nil {
		return ca{}, fmt.Errorf("ca key: %w", err)
	}
	return ca{cert: c, key: k}, nil
}

// IssueServer issues the relay's server certificate for the link host.
func IssueServer(caPair KeyPair, host string, now time.Time) (KeyPair, error) {
	c, err := loadCA(caPair)
	if err != nil {
		return KeyPair{}, err
	}
	k, err := newKey()
	if err != nil {
		return KeyPair{}, err
	}
	sn, err := serial()
	if err != nil {
		return KeyPair{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(serverLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &k.PublicKey, c.key)
	if err != nil {
		return KeyPair{}, err
	}
	kp, err := encodeKey(k)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{CertPEM: encodeCert(der), KeyPEM: kp}, nil
}

// NewCSR creates a client key and a certificate request for it. The relay
// sets the subject itself, so the request carries none.
func NewCSR() (keyPEM string, csrPEM string, err error) {
	k, err := newKey()
	if err != nil {
		return "", "", err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, k)
	if err != nil {
		return "", "", err
	}
	kp, err := encodeKey(k)
	if err != nil {
		return "", "", err
	}
	return kp, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

// parseCSR reads and verifies a PEM certificate request; it accepts ECDSA
// P-256 keys only.
func parseCSR(csrPEM string) (*x509.CertificateRequest, error) {
	b, _ := pem.Decode([]byte(csrPEM))
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("not a PEM certificate request")
	}
	req, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, err
	}
	if err := req.CheckSignature(); err != nil {
		return nil, err
	}
	pub, ok := req.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("certificate request key is not ECDSA P-256")
	}
	return req, nil
}

// issuePeer signs a client certificate for peer from req (CN = peer name).
func issuePeer(caPair KeyPair, req *x509.CertificateRequest, peer string, now time.Time) (certPEM string, notAfter time.Time, err error) {
	c, err := loadCA(caPair)
	if err != nil {
		return "", time.Time{}, err
	}
	sn, err := serial()
	if err != nil {
		return "", time.Time{}, err
	}
	notAfter = now.Add(PeerLifetime)
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: peer},
		NotBefore:    now.Add(-backdate),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, req.PublicKey, c.key)
	if err != nil {
		return "", time.Time{}, err
	}
	// The stored expiry is what the certificate says, to the second.
	return encodeCert(der), tmpl.NotAfter.UTC().Truncate(time.Second), nil
}

// NewSelfSigned creates a self-signed certificate for name, used for the
// gateway's client certificate and the link listener's server certificate.
// Both are pinned by fingerprint on the other side, never chain-verified
// against a public root.
func NewSelfSigned(name string, now time.Time) (KeyPair, error) {
	k, err := newKey()
	if err != nil {
		return KeyPair{}, err
	}
	sn, err := serial()
	if err != nil {
		return KeyPair{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(localLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return KeyPair{}, err
	}
	kp, err := encodeKey(k)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{CertPEM: encodeCert(der), KeyPEM: kp}, nil
}

// needsRenewal reports whether a certificate valid from notBefore to notAfter
// has reached two thirds of its lifetime at now. Lifetime counts from its
// issue, which is backdate after notBefore.
func needsRenewal(notBefore, notAfter, now time.Time) bool {
	issued := notBefore.Add(backdate)
	if !issued.Before(notAfter) {
		issued = notBefore
	}
	life := notAfter.Sub(issued)
	return !now.Before(issued.Add(life * 2 / 3))
}
