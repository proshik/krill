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
	"strconv"
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

	// opMu serializes the operator's changes — role, pairing, forgetting —
	// so that a pairing that answers late cannot undo a role change or a
	// Forget made meanwhile.
	opMu        sync.Mutex
	mu          sync.Mutex // role transitions
	role        atomic.Value
	listener    *GatewayListener
	listenerFP  string
	listenErr   error
	relayErr    error
	clientStop  context.CancelFunc
	clientDone  chan struct{}
	conflictLog atomic.Bool // the last poll found a conflict (log changes only)
	egress      *Egress     // a client's egress, nil until NewEgress wires it
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
	if err := s.renewServerIfDue(ctx); err != nil {
		slog.Error("link: renew the relay's server certificate failed", "err", err)
	}
	go s.maintain()
	return s.apply(ctx)
}

// serverRenewBefore is how long before its expiry the relay's server
// certificate is reissued; maintainEvery is how often that is checked.
const (
	serverRenewBefore = 90 * 24 * time.Hour
	maintainEvery     = 24 * time.Hour
)

// maintain reissues the relay's server certificate before it expires, for as
// long as the service runs.
func (s *Service) maintain() {
	t := time.NewTicker(maintainEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
		renewed, err := s.renewServerIfDueReport(ctx)
		if err != nil {
			slog.Error("link: renew the relay's server certificate failed", "err", err)
		} else if renewed {
			if err := s.apply(ctx); err != nil {
				slog.Error("link: reload after renewing the server certificate failed", "err", err)
			}
		}
		cancel()
	}
}

func (s *Service) renewServerIfDue(ctx context.Context) error {
	_, err := s.renewServerIfDueReport(ctx)
	return err
}

// renewServerIfDueReport reissues the relay's server certificate under the
// same CA when it expires within serverRenewBefore. Clients pin the CA, not
// the server certificate, so they notice nothing.
func (s *Service) renewServerIfDueReport(ctx context.Context) (bool, error) {
	row, err := s.store.Row(ctx)
	if err != nil {
		return false, err
	}
	host := deref(row.LinkHost)
	if row.Role != RoleRelay || host == "" || deref(row.ServerCert) == "" {
		return false, nil
	}
	if !serverDue(*row.ServerCert) {
		return false, nil
	}
	renewed := false
	if err := s.hosts.ReserveLinkHost(ctx, host, func(q *db.Queries) error {
		// Re-read under the lock: the operator may have turned the relay off
		// or moved it while this waited, and that must stand.
		now, err := q.LockLinkSettings(ctx)
		if err != nil {
			return err
		}
		if now.Role != RoleRelay || deref(now.LinkHost) != host || !serverDue(deref(now.ServerCert)) {
			return nil
		}
		ca, err := pair(now.CaCert, now.CaKey)
		if err != nil {
			return fmt.Errorf("link CA: %w", err)
		}
		srv, err := IssueServer(ca, host, time.Now())
		if err != nil {
			return err
		}
		n, err := q.SetLinkServerCert(ctx, db.SetLinkServerCertParams{
			ServerKey: ptr(secret.Enc(srv.KeyPEM)), ServerCert: ptr(srv.CertPEM), Host: host,
		})
		renewed = n == 1
		return err
	}); err != nil {
		return false, err
	}
	if renewed {
		slog.Info("link: relay server certificate reissued", "link_host", host)
	}
	return renewed, nil
}

// serverDue reports whether a relay server certificate is unreadable or
// expires within serverRenewBefore.
func serverDue(certPEM string) bool {
	c, err := ParseCert(certPEM)
	return err != nil || time.Until(c.NotAfter) <= serverRenewBefore
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

// applyAfterWrite runs apply once a role change is stored. The change is
// already visible to the page and the gateway's poll, so the running parts
// must follow even when the request that made it is cancelled meanwhile.
func (s *Service) applyAfterWrite(ctx context.Context) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return s.apply(actx)
}

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
	s.kickEgressLocked()
	if role == RoleOff {
		s.stopAllLocked()
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
	// A role stored before KRILL_LINK_LISTEN_ADDR moved onto a public port
	// does not open it: the listener must be reachable from the gateway only.
	if err := s.ListenRequirement(); err != nil {
		s.stopListenerLocked()
		s.listenErr = err
		slog.Error("link: not listening", "addr", s.cfg.LinkListenAddr, "err", err)
		return nil
	}
	s.ensureListenerLocked(local)
	return nil
}

// stopAllLocked stops the listener, the relay's sessions and the client.
func (s *Service) stopAllLocked() {
	s.role.Store(RoleOff)
	s.relay.unload()
	s.stopClientLocked()
	s.stopListenerLocked()
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

// dispatch hands an authenticated connection to the running role: on a
// relay a link session from its gateway, on a client an app's egress
// connection from the sidecar.
func (s *Service) dispatch(conn net.Conn, hdr ProxyHeader, release func()) {
	switch s.Role() {
	case RoleRelay:
		s.relay.ServeConn(conn, hdr.Src, release)
		return
	case RoleClient:
		if e := s.egressOf(); e != nil {
			e.serveConn(conn, hdr, release)
			return
		}
	}
	_ = conn.Close()
}

func (s *Service) egressOf() *Egress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.egress
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

// ErrLinkPortTaken means KRILL_LINK_LISTEN_ADDR's port is one the firewall
// lockdown already admits from anywhere (SSH, HTTP, HTTPS, the UI port): the
// listener would then be reachable from the world, not from the gateway only.
var ErrLinkPortTaken = errors.New("KRILL_LINK_LISTEN_ADDR uses a port the firewall keeps public")

// ListenRequirement explains why the link listener cannot be used in any role,
// nil when it can.
func (s *Service) ListenRequirement() error {
	port, err := ListenPort(s.cfg.LinkListenAddr)
	if err != nil {
		return err
	}
	public := []int{22, 80, 443}
	if _, ui, err := net.SplitHostPort(s.cfg.ListenAddr); err == nil {
		if p, err := strconv.Atoi(ui); err == nil {
			public = append(public, p)
		}
	}
	for _, p := range public {
		if p == port {
			return ErrLinkPortTaken
		}
	}
	return nil
}

// RelayRequirement explains why this installation cannot be a relay, nil when
// it can.
func (s *Service) RelayRequirement() error {
	if err := s.ListenRequirement(); err != nil {
		return err
	}
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
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.store.q.SetLinkRoleOff(ctx); err != nil {
		return err
	}
	slog.Info("link: role off")
	// Stored: stop now, without reading the settings back — a read failing
	// here would leave the previous role running behind a page that says off.
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopAllLocked()
	s.kickEgressLocked()
	return nil
}

// kickEgressLocked asks egress to reconcile: the sidecar's routes and the
// manifest follow the role.
func (s *Service) kickEgressLocked() {
	if s.egress != nil {
		s.egress.Kick()
	}
}

// SetClient makes the installation a client.
func (s *Service) SetClient(ctx context.Context) error {
	if err := s.ListenRequirement(); err != nil {
		return err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.store.q.SetLinkRoleClient(ctx); err != nil {
		return err
	}
	slog.Info("link: role client")
	return s.applyAfterWrite(ctx)
}

// SetRelay makes the installation a relay routing host (normalized by the
// caller with panel.NormalizeHost). It creates the link CA on first use and a
// server certificate for host, and writes the host under hostshare's host
// lock, refused while a domain or the panel domain holds it.
func (s *Service) SetRelay(ctx context.Context, host string) error {
	if err := s.RelayRequirement(); err != nil {
		return err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.hosts.ReserveLinkHost(ctx, host, func(q *db.Queries) error {
		return s.writeRelay(ctx, q, host, false)
	}); err != nil {
		return err
	}
	slog.Info("link: role relay", "link_host", host)
	return s.applyAfterWrite(ctx)
}

// writeRelay stores the relay role for host. It locks the settings row and
// picks the key material from what that row holds now, so two first
// enablements cannot each create a CA and one replace the other after a
// client pinned it. It keeps the CA (creating one on first use, or when it
// can no longer be decrypted) and reissues the server certificate when the
// host changes, the certificate is unusable or renew is set.
func (s *Service) writeRelay(ctx context.Context, q *db.Queries, host string, renew bool) error {
	row, err := q.LockLinkSettings(ctx)
	if err != nil {
		return fmt.Errorf("lock link settings: %w", err)
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
	if renew || err != nil || deref(row.LinkHost) != host || !issuedBy(srv, ca) || serverDue(srv.CertPEM) {
		if srv, err = IssueServer(ca, host, now); err != nil {
			return err
		}
	}
	return relayWrite(ctx, q, host, ca, srv)
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
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.Role() != RoleClient {
		return ErrWrongRole
	}
	if err := s.client.Pair(ctx, code); err != nil {
		return err
	}
	slog.Info("link: paired with the relay")
	return s.applyAfterWrite(ctx)
}

// ForgetRelay erases the client's key and certificate and drops its session.
func (s *Service) ForgetRelay(ctx context.Context) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
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
	Refusal   *Refusal
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
		if d, ok := s.relay.LastRefusal(p.ID); ok {
			v.Refusal = &d
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
	return s.hosts.LinkHostHolders(ctx, *row.LinkHost, nil)
}

// GatewayTCP is the tcp section for /_krill/gateway/config and the link host
// it routes; nil when there is no route to publish: the role is not relay, the
// relay requirements fail, or the link host is held by the panel, a domain or
// an ingress host — which a release without the link could have handed out.
// That check runs before every publication under the host lock, and the
// configuration is built from the settings re-read under the same lock, so a
// relay turned off or moved meanwhile is not published either.
func (s *Service) GatewayTCP(ctx context.Context) (map[string]any, string, error) {
	row, err := s.store.Row(ctx)
	if err != nil {
		return nil, "", err
	}
	host := deref(row.LinkHost)
	if row.Role != RoleRelay || host == "" {
		return nil, "", nil
	}
	upstream, err := Upstream(s.cfg.AdvertiseAddr, s.cfg.ListenAddr, s.cfg.LinkListenAddr)
	if err != nil || s.ListenRequirement() != nil {
		return nil, "", nil
	}
	var cfg map[string]any
	holders, err := s.hosts.LinkHostHolders(ctx, host, func(q *db.Queries, holders []hostshare.HostHolder) error {
		if len(holders) > 0 {
			return nil
		}
		now, err := q.GetLinkSettings(ctx)
		if err != nil {
			return err
		}
		if now.Role != RoleRelay || deref(now.LinkHost) != host {
			return nil
		}
		local, err := localFromRow(now)
		if err != nil {
			return err
		}
		cfg = GatewayConfig(host, upstream, local)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if len(holders) > 0 {
		if !s.conflictLog.Swap(true) {
			slog.Warn("link: link host is also held by the panel, an app domain or an ingress host; its route is not published", "link_host", host, "holders", len(holders))
		}
		return nil, "", nil
	}
	if s.conflictLog.Swap(false) {
		slog.Info("link: link host conflict resolved; route published", "link_host", host)
	}
	if cfg == nil {
		return nil, "", nil
	}
	return cfg, host, nil
}
