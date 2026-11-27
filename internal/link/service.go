// Package link connects two Krill installations: a client holds one
// persistent outbound TLS 1.3 connection with mutual authentication to a relay,
// and both multiplex streams over it with yamux. This package holds the roles,
// pairing, the transport, reconnection, the relay's endpoint behind its
// gateway and the authenticated gateway listener. Traffic forwarding (egress,
// ingress) arrives in later releases.
package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/secret"
)

// Config is the part of Krill's configuration the link uses.
type Config struct {
	AdvertiseAddr  string
	ListenAddr     string // KRILL_LISTEN_ADDR
	LinkListenAddr string // KRILL_LINK_LISTEN_ADDR
	Version        string
}

// ErrWrongRole refuses an action the current role does not have.
var ErrWrongRole = errors.New("not available in the current role")

// ErrInvalidAddr is a pairing address that is not host:port.
var ErrInvalidAddr = errors.New("not a host:port address")

// Service runs the installation's link role: the gateway listener while the
// role is not off, the relay's sessions or the client's session.
type Service struct {
	cfg   Config
	store *Store
	hosts *hostshare.Store

	ctx    context.Context
	cancel context.CancelFunc
	relay  *Relay
	client *Client

	mu          sync.Mutex // role transitions
	role        atomic.Value
	listener    *GatewayListener
	listenerFP  string
	listenErr   error
	relayErr    error
	clientStop  context.CancelFunc
	clientDone  chan struct{}
	conflictLog atomic.Bool // the last poll found a conflict (log changes only)
}

// NewService creates the link service; Start runs it.
func NewService(pool *pgxpool.Pool, hosts *hostshare.Store, cfg Config) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	st := NewStore(pool)
	s := &Service{cfg: cfg, store: st, hosts: hosts, ctx: ctx, cancel: cancel,
		relay: newRelay(ctx, st, cfg.Version), client: newClient(st, cfg.Version)}
	s.role.Store(RoleOff)
	return s
}

// Store is the service's store (tests).
func (s *Service) Store() *Store { return s.store }

// Start creates the settings row and brings the stored role up.
func (s *Service) Start(ctx context.Context) error {
	if err := s.store.Init(ctx); err != nil {
		return fmt.Errorf("init link settings: %w", err)
	}
	return s.apply(ctx)
}

// Close stops the listener and every session.
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel()
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}
	s.relay.unload()
	s.stopClientLocked()
}

// Role is the role currently running.
func (s *Service) Role() string { return s.role.Load().(string) }

// apply brings the running parts in line with the stored role.
func (s *Service) apply(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil
	}
	row, err := s.store.Row(ctx)
	if err != nil {
		return fmt.Errorf("read link settings: %w", err)
	}
	role := row.Role
	if role == RoleOff {
		s.role.Store(RoleOff)
		s.relay.unload()
		s.stopClientLocked()
		s.stopListenerLocked()
		return nil
	}
	local, err := s.store.EnsureLocalMaterial(ctx)
	if err != nil {
		return fmt.Errorf("link gateway certificates: %w", err)
	}
	if role == RoleRelay {
		s.stopClientLocked()
		s.relayErr = s.relay.load(ctx)
		if s.relayErr != nil {
			slog.Error("link: relay certificates unusable", "err", s.relayErr)
		}
	} else {
		s.relay.unload()
		s.relayErr = nil
		s.startClientLocked()
	}
	s.role.Store(role)
	s.ensureListenerLocked(local)
	return nil
}

func (s *Service) ensureListenerLocked(local LocalMaterial) {
	fp, _ := CertFingerprint(local.Gateway.CertPEM)
	if s.listener != nil && s.listenerFP == fp {
		return
	}
	s.stopListenerLocked()
	l, err := ListenGateway(s.cfg.LinkListenAddr, local, s.dispatch)
	if err != nil {
		s.listenErr = err
		slog.Error("link: cannot listen", "addr", s.cfg.LinkListenAddr, "err", err)
		return
	}
	s.listener, s.listenerFP, s.listenErr = l, fp, nil
	slog.Info("link: gateway listener up", "addr", l.Addr().String())
}

func (s *Service) stopListenerLocked() {
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener, s.listenerFP = nil, ""
	}
	s.listenErr = nil
}

// dispatch hands an authenticated connection to the running role.
func (s *Service) dispatch(conn net.Conn, src netip.AddrPort) {
	if s.Role() == RoleRelay {
		s.relay.ServeConn(conn, src)
		return
	}
	// A client's listener has nothing to serve until egress (KRL-002).
	_ = conn.Close()
}

func (s *Service) startClientLocked() {
	if s.clientStop != nil {
		s.client.Wake()
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan struct{})
	s.clientStop, s.clientDone = cancel, done
	go func() {
		defer close(done)
		s.client.run(ctx)
	}()
}

func (s *Service) stopClientLocked() {
	if s.clientStop == nil {
		return
	}
	s.clientStop()
	s.client.Wake()
	<-s.clientDone
	s.clientStop, s.clientDone = nil, nil
}

// RelayRequirement explains why this installation cannot be a relay, nil when
// it can.
func (s *Service) RelayRequirement() error {
	_, err := Upstream(s.cfg.AdvertiseAddr, s.cfg.ListenAddr, s.cfg.LinkListenAddr)
	return err
}

// ListenError is why the gateway listener is not up, nil when it is (or the
// role is off).
func (s *Service) ListenError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listenErr
}

// RelayError is why a relay cannot serve (its certificates), nil otherwise.
func (s *Service) RelayError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.relayErr
}

// ActivePort is the link listener's port while the role is not off, 0 when it
// is: the firewall keeps it open to the gateway only then.
func (s *Service) ActivePort(ctx context.Context) (int, error) {
	row, err := s.store.Row(ctx)
	if err != nil {
		return 0, err
	}
	if row.Role == RoleOff {
		return 0, nil
	}
	return ListenPort(s.cfg.LinkListenAddr)
}

// Settings is the stored link settings, keys still encrypted.
func (s *Service) Settings(ctx context.Context) (db.LinkSetting, error) { return s.store.Row(ctx) }

// SetOff turns the link off: the listener closes and every session ends. Key
// material and peers stay, so turning the role back on needs no re-pairing.
func (s *Service) SetOff(ctx context.Context) error {
	if err := s.store.q.SetLinkRoleOff(ctx); err != nil {
		return err
	}
	slog.Info("link: role off")
	return s.apply(ctx)
}

// SetClient makes the installation a client.
func (s *Service) SetClient(ctx context.Context) error {
	if err := s.store.q.SetLinkRoleClient(ctx); err != nil {
		return err
	}
	slog.Info("link: role client")
	return s.apply(ctx)
}

// SetRelay makes the installation a relay routing host (normalized by the
// caller with panel.NormalizeHost). It creates the link CA on first use and a
// server certificate for host, and writes the host under hostshare's host
// lock, refused while a domain or the panel domain holds it.
func (s *Service) SetRelay(ctx context.Context, host string) error {
	if err := s.RelayRequirement(); err != nil {
		return err
	}
	row, err := s.store.Row(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	ca, err := pair(row.CaCert, row.CaKey)
	if err != nil {
		if errors.Is(err, secret.ErrUndecryptable) {
			slog.Warn("link: the link CA cannot be decrypted with the current KRILL_SECRET_KEY; creating a new one (every client must pair again)")
		}
		if ca, err = NewCA(now); err != nil {
			return err
		}
	}
	srv, err := pair(row.ServerCert, row.ServerKey)
	if err != nil || deref(row.LinkHost) != host || !issuedBy(srv, ca) {
		if srv, err = IssueServer(ca, host, now); err != nil {
			return err
		}
	}
	if err := s.hosts.ReserveLinkHost(ctx, host, func(q *db.Queries) error {
		return relayWrite(ctx, q, host, ca, srv)
	}); err != nil {
		return err
	}
	slog.Info("link: role relay", "link_host", host)
	return s.apply(ctx)
}

// issuedBy reports whether srv was signed by ca.
func issuedBy(srv, ca KeyPair) bool {
	c, err := ParseCert(srv.CertPEM)
	if err != nil {
		return false
	}
	root, err := ParseCert(ca.CertPEM)
	if err != nil {
		return false
	}
	return c.CheckSignatureFrom(root) == nil
}

// CreatePairingCode creates a one-time code for a new peer. addr is where the
// client dials, "" for the link host on 443.
func (s *Service) CreatePairingCode(ctx context.Context, peer, addr string) (string, error) {
	if s.Role() != RoleRelay {
		return "", ErrWrongRole
	}
	m, err := s.store.Relay(ctx)
	if err != nil {
		return "", err
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = net.JoinHostPort(m.Host, "443")
	}
	if ValidAddr(addr) != nil {
		return "", ErrInvalidAddr
	}
	caFP, err := CertFingerprint(m.CA.CertPEM)
	if err != nil {
		return "", err
	}
	sec, err := s.store.CreateCode(ctx, peer)
	if err != nil {
		return "", err
	}
	slog.Info("link: pairing code created", "peer", peer)
	return PairingCode{Addr: addr, SNI: m.Host, CASHA256: caFP, Secret: sec}.Encode(), nil
}

// RemovePeer deletes a peer and drops its session.
func (s *Service) RemovePeer(ctx context.Context, id int64) error {
	if err := s.relay.RemovePeer(ctx, id); err != nil {
		return err
	}
	slog.Info("link: peer removed", "peer_id", id)
	return nil
}

// Pair redeems a pairing code from the relay operator.
func (s *Service) Pair(ctx context.Context, code string) error {
	if s.Role() != RoleClient {
		return ErrWrongRole
	}
	if err := s.client.Pair(ctx, code); err != nil {
		return err
	}
	slog.Info("link: paired with the relay")
	return s.apply(ctx)
}

// ForgetRelay erases the client's key and certificate and drops its session.
func (s *Service) ForgetRelay(ctx context.Context) error {
	if err := s.store.Forget(ctx); err != nil {
		return err
	}
	s.client.Wake()
	slog.Info("link: relay forgotten")
	return nil
}

// ClientStatus is the client's link state.
func (s *Service) ClientStatus() ClientStatus { return s.client.Status() }

// PeerView is a relay's peer with its live state.
type PeerView struct {
	Peer      db.LinkPeer
	Session   PeerSession
	Connected bool
	Duplicate *DuplicateAttempt
}

// Peers lists the relay's peers with their sessions.
func (s *Service) Peers(ctx context.Context) ([]PeerView, error) {
	rows, err := s.store.Peers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PeerView, 0, len(rows))
	for _, p := range rows {
		v := PeerView{Peer: p}
		v.Session, v.Connected = s.relay.Session(p.ID)
		if d, ok := s.relay.Duplicate(p.ID); ok {
			v.Duplicate = &d
		}
		out = append(out, v)
	}
	return out, nil
}

// OpenCodes lists pairing codes waiting to be redeemed.
func (s *Service) OpenCodes(ctx context.Context) ([]db.ListOpenLinkPairingCodesRow, error) {
	return s.store.OpenCodes(ctx)
}

// LinkHostConflicts re-checks, under the host lock, what holds the relay's
// link host besides the link: the panel domain or app domains. Empty for a
// free host or another role.
func (s *Service) LinkHostConflicts(ctx context.Context) ([]hostshare.HostHolder, error) {
	row, err := s.store.Row(ctx)
	if err != nil {
		return nil, err
	}
	if row.Role != RoleRelay || deref(row.LinkHost) == "" {
		return nil, nil
	}
	return s.hosts.LinkHostHolders(ctx, *row.LinkHost)
}

// GatewayTCP is the tcp section for /_krill/gateway/config, nil when there is
// no route to publish: the role is not relay, the relay requirements fail, or
// the link host is held by the panel or a domain — which a release without
// the link could have handed out. That check runs before every publication,
// under the host lock.
func (s *Service) GatewayTCP(ctx context.Context) (map[string]any, error) {
	row, err := s.store.Row(ctx)
	if err != nil {
		return nil, err
	}
	host := deref(row.LinkHost)
	if row.Role != RoleRelay || host == "" {
		return nil, nil
	}
	upstream, err := Upstream(s.cfg.AdvertiseAddr, s.cfg.ListenAddr, s.cfg.LinkListenAddr)
	if err != nil {
		return nil, nil
	}
	holders, err := s.hosts.LinkHostHolders(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(holders) > 0 {
		if !s.conflictLog.Swap(true) {
			slog.Warn("link: link host is also held by the panel or an app domain; its route is not published", "link_host", host, "holders", len(holders))
		}
		return nil, nil
	}
	if s.conflictLog.Swap(false) {
		slog.Info("link: link host conflict resolved; route published", "link_host", host)
	}
	local, err := s.store.Local(ctx)
	if err != nil {
		return nil, err
	}
	return GatewayConfig(host, upstream, local), nil
}
