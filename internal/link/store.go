package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
)

// Roles of an installation.
const (
	RoleOff    = "off"
	RoleClient = "client"
	RoleRelay  = "relay"
)

// ErrCodeRefused is a pairing attempt with an unknown, expired or already
// redeemed code (by another key). Deliberately one error: the attempt carries
// a credential and the answer must not help guess it.
var ErrCodeRefused = errors.New("pairing code refused")

// ErrPeerExists refuses a pairing code for a name a peer already has.
var ErrPeerExists = errors.New("a peer with that name exists")

// ErrNotPaired is a client with no relay to connect to.
var ErrNotPaired = errors.New("not paired with a relay")

// Store reads and writes the link's rows. Private keys pass through
// secret.Enc/Dec here and nowhere else.
type Store struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

// NewStore wraps the pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, q: db.New(pool)} }

// Init creates the settings row.
func (s *Store) Init(ctx context.Context) error { return s.q.InitLinkSettings(ctx) }

// Row is the stored settings, keys still encrypted.
func (s *Store) Row(ctx context.Context) (db.LinkSetting, error) {
	row, err := s.q.GetLinkSettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.LinkSetting{Role: RoleOff}, nil
	}
	return row, err
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptr(s string) *string { return &s }

// pair decrypts a stored key and pairs it with its certificate.
func pair(cert, key *string) (KeyPair, error) {
	if deref(cert) == "" || deref(key) == "" {
		return KeyPair{}, errMissing
	}
	k, err := secret.Dec(*key)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{CertPEM: *cert, KeyPEM: k}, nil
}

var errMissing = errors.New("key material missing")

func (s *Store) inTx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // no-op after Commit
	if err := fn(db.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LocalMaterial is the gateway's client certificate towards the listener and
// the listener's own certificate.
type LocalMaterial struct {
	Gateway  KeyPair
	Listener KeyPair
}

// EnsureLocalMaterial returns the gateway and listener certificates, creating
// both when they are missing or cannot be decrypted any more (a changed
// KRILL_SECRET_KEY). Nothing outside this installation holds them, so a new
// pair costs only the gateway's next poll.
func (s *Store) EnsureLocalMaterial(ctx context.Context) (LocalMaterial, error) {
	var out LocalMaterial
	err := s.inTx(ctx, func(q *db.Queries) error {
		row, err := q.LockLinkSettings(ctx)
		if err != nil {
			return fmt.Errorf("lock link settings: %w", err)
		}
		gw, gerr := pair(row.GatewayCert, row.GatewayKey)
		ls, lerr := pair(row.ListenerCert, row.ListenerKey)
		if gerr == nil && lerr == nil {
			out = LocalMaterial{Gateway: gw, Listener: ls}
			return nil
		}
		if errors.Is(gerr, secret.ErrUndecryptable) || errors.Is(lerr, secret.ErrUndecryptable) {
			slog.Warn("link gateway certificates cannot be decrypted with the current KRILL_SECRET_KEY; replacing them")
		}
		now := time.Now()
		if gw, err = NewSelfSigned("krill-link-gateway", now); err != nil {
			return err
		}
		if ls, err = NewSelfSigned(ListenerServerName, now); err != nil {
			return err
		}
		out = LocalMaterial{Gateway: gw, Listener: ls}
		return q.SetLinkGatewayMaterial(ctx, db.SetLinkGatewayMaterialParams{
			GatewayKey: ptr(secret.Enc(gw.KeyPEM)), GatewayCert: ptr(gw.CertPEM),
			ListenerKey: ptr(secret.Enc(ls.KeyPEM)), ListenerCert: ptr(ls.CertPEM),
		})
	})
	return out, err
}

// RelayMaterial is what a relay serves with.
type RelayMaterial struct {
	Host   string
	CA     KeyPair
	Server KeyPair
}

// Relay reads the relay's CA and server certificate.
func (s *Store) Relay(ctx context.Context) (RelayMaterial, error) {
	row, err := s.Row(ctx)
	if err != nil {
		return RelayMaterial{}, err
	}
	ca, err := pair(row.CaCert, row.CaKey)
	if err != nil {
		return RelayMaterial{}, fmt.Errorf("link CA: %w", err)
	}
	srv, err := pair(row.ServerCert, row.ServerKey)
	if err != nil {
		return RelayMaterial{}, fmt.Errorf("link server certificate: %w", err)
	}
	return RelayMaterial{Host: deref(row.LinkHost), CA: ca, Server: srv}, nil
}

// relayWrite stores the relay role with its link host, CA and server
// certificate; it runs inside hostshare.ReserveLinkHost.
func relayWrite(ctx context.Context, q *db.Queries, host string, ca, srv KeyPair) error {
	return q.SetLinkRelay(ctx, db.SetLinkRelayParams{
		LinkHost: ptr(host),
		CaKey:    ptr(secret.Enc(ca.KeyPEM)), CaCert: ptr(ca.CertPEM),
		ServerKey: ptr(secret.Enc(srv.KeyPEM)), ServerCert: ptr(srv.CertPEM),
	})
}

// serverCertWrite stores a reissued relay server certificate while the
// settings still name this relay and host, and reports whether it did; it
// runs inside hostshare.ReserveLinkHost.
func serverCertWrite(ctx context.Context, q *db.Queries, host string, srv KeyPair) (bool, error) {
	n, err := q.SetLinkServerCert(ctx, db.SetLinkServerCertParams{
		ServerKey: ptr(secret.Enc(srv.KeyPEM)), ServerCert: ptr(srv.CertPEM), Host: host,
	})
	return n == 1, err
}

// ClientMaterial is a paired client's relay and identity.
type ClientMaterial struct {
	Addr     string
	SNI      string
	CASHA256 string
	Identity KeyPair
}

// Client reads the client's pairing. ErrNotPaired when there is none.
func (s *Store) Client(ctx context.Context) (ClientMaterial, error) {
	row, err := s.Row(ctx)
	if err != nil {
		return ClientMaterial{}, err
	}
	if deref(row.RelayAddr) == "" {
		return ClientMaterial{}, ErrNotPaired
	}
	id, err := pair(row.ClientCert, row.ClientKey)
	if errors.Is(err, errMissing) {
		return ClientMaterial{}, ErrNotPaired
	}
	if err != nil {
		return ClientMaterial{}, fmt.Errorf("link client key: %w", err)
	}
	return ClientMaterial{Addr: *row.RelayAddr, SNI: deref(row.RelaySni), CASHA256: deref(row.RelayCaSha256), Identity: id}, nil
}

// SavePaired stores a fresh pairing and makes the installation a client.
func (s *Store) SavePaired(ctx context.Context, c PairingCode, id KeyPair) error {
	return s.q.SetLinkClientPaired(ctx, db.SetLinkClientPairedParams{
		RelayAddr: ptr(c.Addr), RelaySni: ptr(c.SNI), RelayCaSha256: ptr(c.CASHA256),
		ClientKey: ptr(secret.Enc(id.KeyPEM)), ClientCert: ptr(id.CertPEM),
	})
}

// SaveRenewed replaces the client's certificate, unless it changed meanwhile
// (a new pairing).
func (s *Store) SaveRenewed(ctx context.Context, oldCert string, id KeyPair) (bool, error) {
	n, err := s.q.SetLinkClientCert(ctx, db.SetLinkClientCertParams{
		ClientKey: ptr(secret.Enc(id.KeyPEM)), ClientCert: ptr(id.CertPEM), OldCert: oldCert,
	})
	return n == 1, err
}

// Forget erases the client's relay, key and certificate.
func (s *Store) Forget(ctx context.Context) error { return s.q.ForgetLinkRelay(ctx) }

// CreateCode creates a one-time pairing code for a new peer and returns its
// secret; only the secret's SHA-256 is stored.
func (s *Store) CreateCode(ctx context.Context, peer string) (string, error) {
	n, err := s.q.CountLinkPeersByName(ctx, peer)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return "", ErrPeerExists
	}
	sec, err := NewSecret()
	if err != nil {
		return "", err
	}
	if _, err := s.q.CreateLinkPairingCode(ctx, db.CreateLinkPairingCodeParams{
		PeerName: peer, SecretSha256: SecretSHA256(sec), LifetimeSeconds: CodeLifetime.Seconds(),
	}); err != nil {
		return "", err
	}
	if err := s.q.DeleteStaleLinkPairingCodes(ctx); err != nil {
		slog.Warn("link: delete stale pairing codes failed", "err", err)
	}
	return sec, nil
}

// Redeem redeems a pairing code for the key in csrPEM and creates the peer, in
// one transaction. The code row is locked and then updated conditionally, so
// of two attempts only one wins. A retry with the same key before expiry —
// its answer may have been lost after the commit — gets the same certificate;
// any other key is refused.
func (s *Store) Redeem(ctx context.Context, secretPlain, csrPEM string, ca KeyPair) (certPEM, peer string, err error) {
	req, err := parseCSR(csrPEM)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrCodeRefused, err)
	}
	keyHash, err := publicKeySHA256(req.PublicKey)
	if err != nil {
		return "", "", err
	}
	hash := SecretSHA256(secretPlain)
	err = s.inTx(ctx, func(q *db.Queries) error {
		code, err := q.LockLinkPairingCode(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCodeRefused
		}
		if err != nil {
			return err
		}
		peer = code.PeerName
		live, err := q.LinkPairingCodeLive(ctx, code.ID)
		if err != nil {
			return err
		}
		if !live {
			return ErrCodeRefused
		}
		if code.UsedAt.Valid {
			if deref(code.KeySha256) != keyHash || deref(code.IssuedCert) == "" {
				return ErrCodeRefused
			}
			certPEM = *code.IssuedCert
			return nil
		}
		issued, notAfter, err := issuePeer(ca, req, code.PeerName, time.Now())
		if err != nil {
			return err
		}
		n, err := q.RedeemLinkPairingCode(ctx, db.RedeemLinkPairingCodeParams{
			SecretSha256: hash, KeySha256: ptr(keyHash), IssuedCert: ptr(issued),
		})
		if err != nil {
			return err
		}
		if n != 1 {
			// Expired between the read and the update.
			return ErrCodeRefused
		}
		fp, err := CertFingerprint(issued)
		if err != nil {
			return err
		}
		if _, err := q.CreateLinkPeer(ctx, db.CreateLinkPeerParams{Name: code.PeerName, CertSha256: fp, CertNotAfter: notAfter}); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == "23505" {
				return ErrPeerExists
			}
			return err
		}
		certPEM = issued
		return nil
	})
	if err != nil {
		return "", peer, err
	}
	return certPEM, peer, nil
}

// Renew issues a new certificate for a connected peer. The certificate the
// session holds stays valid until the first connect with the new one.
func (s *Store) Renew(ctx context.Context, peer db.LinkPeer, sessionFP, csrPEM string, ca KeyPair) (string, error) {
	req, err := parseCSR(csrPEM)
	if err != nil {
		return "", err
	}
	certPEM, notAfter, err := issuePeer(ca, req, peer.Name, time.Now())
	if err != nil {
		return "", err
	}
	fp, err := CertFingerprint(certPEM)
	if err != nil {
		return "", err
	}
	n, err := s.q.RenewLinkPeerCert(ctx, db.RenewLinkPeerCertParams{
		ID: peer.ID, CertNotAfter: notAfter, SessionFingerprint: sessionFP, NewFingerprint: fp,
	})
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", ErrPeerGone
	}
	return certPEM, nil
}

// PeerByFingerprint finds the peer holding a certificate (current or the one
// it is renewing away from).
func (s *Store) PeerByFingerprint(ctx context.Context, fp string) (db.LinkPeer, error) {
	p, err := s.q.GetLinkPeerByFingerprint(ctx, fp)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.LinkPeer{}, ErrPeerGone
	}
	return p, err
}

// Peer re-reads a peer by ID.
func (s *Store) Peer(ctx context.Context, id int64) (db.LinkPeer, error) {
	p, err := s.q.GetLinkPeer(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.LinkPeer{}, ErrPeerGone
	}
	return p, err
}

// Peers lists the relay's peers.
func (s *Store) Peers(ctx context.Context) ([]db.LinkPeer, error) { return s.q.ListLinkPeers(ctx) }

// OpenCodes lists pairing codes not yet redeemed or expired.
func (s *Store) OpenCodes(ctx context.Context) ([]db.ListOpenLinkPairingCodesRow, error) {
	return s.q.ListOpenLinkPairingCodes(ctx)
}

func (s *Store) markConnected(ctx context.Context, id int64, fp string) error {
	return s.q.MarkLinkPeerConnected(ctx, db.MarkLinkPeerConnectedParams{ID: id, Fingerprint: fp})
}

func (s *Store) markDisconnected(ctx context.Context, id int64) error {
	return s.q.MarkLinkPeerDisconnected(ctx, id)
}

func (s *Store) deletePeer(ctx context.Context, id int64) error {
	n, err := s.q.DeleteLinkPeer(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrPeerGone
	}
	return nil
}

// Local reads the gateway and listener certificates without creating them.
func (s *Store) Local(ctx context.Context) (LocalMaterial, error) {
	row, err := s.Row(ctx)
	if err != nil {
		return LocalMaterial{}, err
	}
	return localFromRow(row)
}

func localFromRow(row db.LinkSetting) (LocalMaterial, error) {
	gw, err := pair(row.GatewayCert, row.GatewayKey)
	if err != nil {
		return LocalMaterial{}, fmt.Errorf("gateway certificate: %w", err)
	}
	ls, err := pair(row.ListenerCert, row.ListenerKey)
	if err != nil {
		return LocalMaterial{}, fmt.Errorf("listener certificate: %w", err)
	}
	return LocalMaterial{Gateway: gw, Listener: ls}, nil
}
