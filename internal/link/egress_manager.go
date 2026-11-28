package link

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/panel"
)

// Sidecar reconciliation timing; variables for tests.
var (
	sidecarConverge  = 3 * time.Minute
	sidecarPoll      = time.Second
	egressReconcile  = time.Minute            // periodic pass: networks of deleted apps
	indexRefreshMin  = 250 * time.Millisecond // a missed address refreshes at most this often
	prepareRemembers = 30 * time.Minute       // a Prepare whose deploy never reports back
)

// ErrUnknownSource refuses a connection whose addresses are not an app's task
// talking to the sidecar on that app's egress network.
var ErrUnknownSource = errors.New("the connection does not come from a task of the app whose egress network it arrived on")

// ErrEgressUnsupported: the docker engine cannot manage the egress networks
// and the sidecar.
var ErrEgressUnsupported = errors.New("the docker engine cannot manage the link's egress sidecar")

// egressEngine is what egress needs from the docker engine.
type egressEngine interface {
	docker.Engine
	docker.PrefixNetworks
	docker.SwarmObjects
}

// Egress runs a client's egress: it reconciles the egress networks and the
// sidecar, identifies the app behind each connection the sidecar hands over
// and decides it, and opens the egress stream through the link.
type Egress struct {
	svc   *Service
	store *Store
	eng   egressEngine
	nets  docker.PrefixNetworks // eng; apart so identification can be tested alone
	ctx   context.Context
	names egressNames

	providerURL string // the sidecar's dynamic configuration, on Krill's UI listener
	token       string
	upstream    string // the link listener, as the sidecar reaches it
	upErr       error  // why the sidecar cannot reach Krill

	recMu sync.Mutex // one reconcile at a time
	kick  chan struct{}

	mu        sync.Mutex
	preparing map[int64]time.Time
	idx       egressIndex
	idxAt     time.Time
	refreshMu sync.Mutex

	traffic *traffic
}

// NewEgress wires egress into the link service. eng must manage networks,
// services and Swarm secrets (the real engine); gatewaySecret is the
// gateway's stored secret the sidecar's token is derived from.
func NewEgress(svc *Service, eng docker.Engine, gatewaySecret string) (*Egress, error) {
	ee, ok := eng.(egressEngine)
	if !ok {
		return nil, ErrEgressUnsupported
	}
	e := &Egress{svc: svc, store: svc.store, eng: ee, nets: ee, ctx: svc.ctx, names: defaultNames, token: EgressToken(gatewaySecret),
		kick: make(chan struct{}, 1), preparing: map[int64]time.Time{}, traffic: newTraffic()}
	ui, err := panel.Upstream(svc.cfg.AdvertiseAddr, svc.cfg.ListenAddr)
	if err == nil {
		e.providerURL = ui + EgressConfigPath
		e.upstream, err = Upstream(svc.cfg.AdvertiseAddr, svc.cfg.ListenAddr, svc.cfg.LinkListenAddr)
	}
	e.upErr = err
	svc.mu.Lock()
	svc.egress = e
	svc.mu.Unlock()
	return e, nil
}

// Requirement explains why the sidecar cannot reach Krill (the same needs as
// a relay's gateway), nil when it can.
func (e *Egress) Requirement() error { return e.upErr }

// Token is the sidecar's provider token.
func (e *Egress) Token() string { return e.token }

// Run reconciles the sidecar at start, whenever kicked, and periodically,
// until the service closes.
func (e *Egress) Run() {
	t := time.NewTicker(egressReconcile)
	defer t.Stop()
	for {
		ctx, cancel := context.WithTimeout(e.ctx, sidecarConverge+time.Minute)
		if err := e.reconcile(ctx, nil); err != nil && e.ctx.Err() == nil {
			slog.Error("link: egress sidecar reconcile failed", "err", err)
		}
		cancel()
		e.publishManifest(e.ctx)
		select {
		case <-e.ctx.Done():
			return
		case <-e.kick:
		case <-t.C:
		}
	}
}

// Kick asks for a reconcile soon.
func (e *Egress) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Prepare implements deploy.EgressHook: it creates the app's egress network
// and has the sidecar answer for dests there, and returns once the sidecar's
// task with them runs.
func (e *Egress) Prepare(ctx context.Context, appID int64, dests []string) error {
	req := &prepareReq{app: appID}
	for _, s := range dests {
		d, err := ParseDestinationString(s)
		if err != nil {
			return fmt.Errorf("destination %q: %w", s, err)
		}
		req.dests = append(req.dests, d)
	}
	e.mu.Lock()
	e.preparing[appID] = time.Now()
	e.mu.Unlock()
	return e.reconcile(ctx, req)
}

// Changed implements deploy.EgressHook: a deploy of the app finished.
func (e *Egress) Changed(appID int64) {
	e.mu.Lock()
	delete(e.preparing, appID)
	e.mu.Unlock()
	e.Kick()
}

func (e *Egress) preparingSet() map[int64]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[int64]bool{}
	for id, at := range e.preparing {
		if time.Since(at) > prepareRemembers {
			delete(e.preparing, id)
			continue
		}
		out[id] = true
	}
	return out
}

// reconcile brings the egress networks and the sidecar in line with the
// apps: the sidecar stays on every network an app uses, with that app's names;
// once nothing uses a network the sidecar leaves it and it is removed; with
// none left the sidecar itself is removed. Removing it any earlier would let
// the names resolve to the real addresses.
func (e *Egress) reconcile(ctx context.Context, prep *prepareReq) error {
	e.recMu.Lock()
	defer e.recMu.Unlock()
	if prep != nil {
		if _, err := e.eng.NetworkEnsure(ctx, e.names.network(prep.app)); err != nil {
			return fmt.Errorf("create the egress network: %w", err)
		}
	}
	nets, err := e.eng.PrefixNetworks(ctx, e.names.netPrefix)
	if err != nil {
		return fmt.Errorf("list the egress networks: %w", err)
	}
	e.setIndex(nets)
	apps, err := e.store.egressApps(ctx)
	if err != nil {
		return err
	}
	saved := map[int64][]Destination{}
	for id, a := range apps {
		saved[id] = a.Saved
	}
	var ids []int64
	for _, n := range nets {
		if id, ok := e.names.appOf(n.Name); ok {
			ids = append(ids, id)
		}
	}
	applied, err := e.store.appliedSelections(ctx, ids)
	if err != nil {
		return err
	}
	plan := planSidecar(e.names, nets, saved, applied, e.preparingSet(), prep)
	if len(plan.Keep) == 0 {
		if err := e.removeSidecar(ctx); err != nil {
			return err
		}
		e.removeNetworks(ctx, plan.Unused)
		return nil
	}
	if e.upErr != nil {
		return fmt.Errorf("the egress sidecar cannot reach Krill: %w", e.upErr)
	}
	in := plan.input(e.providerURL, e.token)
	in.Name = e.names.sidecar
	spec, static := sidecarSpec(in)
	labels := map[string]string{sidecarObjectLabel: e.names.objects}
	if err := e.eng.SecretEnsure(ctx, spec.Secrets[0].Name, static, labels); err != nil {
		return fmt.Errorf("store the sidecar configuration: %w", err)
	}
	changed, err := e.deploySidecar(ctx, spec)
	if err != nil {
		return err
	}
	if changed {
		if err := e.eng.PruneObjects(ctx, sidecarObjectLabel, e.names.objects, []string{spec.Secrets[0].Name}); err != nil {
			slog.Warn("link: prune old sidecar configurations failed", "err", err)
		}
		if nets, err := e.eng.PrefixNetworks(ctx, e.names.netPrefix); err == nil {
			e.setIndex(nets)
		}
	}
	e.removeNetworks(ctx, plan.Unused)
	return nil
}

// deploySidecar deploys spec unless the running sidecar already has it and
// is settled, and waits for its new task; it reports whether it deployed.
func (e *Egress) deploySidecar(ctx context.Context, spec docker.ServiceSpec) (bool, error) {
	before, err := e.eng.ServiceProgress(ctx, e.names.sidecar, nil)
	if err != nil {
		return false, fmt.Errorf("read the sidecar's tasks: %w", err)
	}
	if before.Found && docker.UpdateSettled(before.UpdateState) && before.Running > 0 {
		if cur, found, err := e.eng.ServiceLabels(ctx, e.names.sidecar); err == nil && found {
			if h := spec.Labels[sidecarHashLabel]; h != "" && cur[sidecarHashLabel] == h {
				return false, nil
			}
		}
	}
	var baseline []string
	if before.Found {
		baseline = before.TaskIDs
	}
	slog.Info("link: deploying the egress sidecar", "networks", len(spec.Networks))
	if err := e.eng.ServiceDeploy(ctx, spec); err != nil {
		return false, fmt.Errorf("deploy the egress sidecar: %w", err)
	}
	if err := waitService(ctx, e.eng, e.names.sidecar, baseline); err != nil {
		return true, err
	}
	slog.Info("link: egress sidecar deployed", "networks", len(spec.Networks))
	return true, nil
}

// waitService polls until a task of name outside baseline runs and Swarm
// has completed the update (as traefik.Reconcile waits for the gateway).
func waitService(ctx context.Context, eng docker.Engine, name string, baseline []string) error {
	cctx, cancel := context.WithTimeout(ctx, sidecarConverge)
	defer cancel()
	var lastErr error
	for first := true; ; first = false {
		p, err := eng.ServiceProgress(cctx, name, baseline)
		switch {
		case err != nil:
			lastErr = err
		case p.Found && !first && strings.HasPrefix(p.UpdateState, "rollback"):
			return fmt.Errorf("%s update rolled back by Swarm", name)
		case p.Found && p.Desired > 0 && p.Running >= p.Desired && docker.UpdateSettled(p.UpdateState):
			return nil
		}
		select {
		case <-cctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if lastErr != nil {
				return fmt.Errorf("%s did not start within %s: %w", name, sidecarConverge, lastErr)
			}
			return fmt.Errorf("%s did not start within %s (see docker service ps %s)", name, sidecarConverge, name)
		case <-time.After(sidecarPoll):
		}
	}
}

// removeSidecar removes the sidecar service and its configurations.
func (e *Egress) removeSidecar(ctx context.Context) error {
	st, err := e.eng.ServiceState(ctx, e.names.sidecar)
	if err != nil {
		return fmt.Errorf("read the sidecar: %w", err)
	}
	if st.Found {
		slog.Info("link: no app uses an egress network any more; removing the egress sidecar")
		if err := e.eng.ServiceRemove(ctx, e.names.sidecar); err != nil {
			return fmt.Errorf("remove the egress sidecar: %w", err)
		}
	}
	if err := e.eng.PruneObjects(ctx, sidecarObjectLabel, e.names.objects, nil); err != nil {
		slog.Warn("link: prune sidecar configurations failed", "err", err)
	}
	return nil
}

// removeNetworks removes egress networks nothing uses; one still in use (a
// task shutting down) is tried again on a later pass.
func (e *Egress) removeNetworks(ctx context.Context, names []string) {
	for _, n := range names {
		if err := e.eng.NetworkRemove(ctx, n); err != nil {
			slog.Info("link: egress network not removed yet", "network", n, "err", err)
			continue
		}
		slog.Info("link: egress network removed", "network", n)
	}
}

func (e *Egress) setIndex(nets []docker.PrefixNetwork) {
	x := buildIndex(e.names, nets)
	e.mu.Lock()
	e.idx, e.idxAt = x, time.Now()
	e.mu.Unlock()
}

// identify finds the app behind a connection from the sidecar: its PROXY
// destination is the sidecar's address on that app's egress network, and its
// source must be a task of that app on that network. A miss refreshes the
// addresses once (a task that just started), then refuses.
func (e *Egress) identify(ctx context.Context, src, dst netip.Addr) (int64, error) {
	e.mu.Lock()
	x, at := e.idx, e.idxAt
	e.mu.Unlock()
	if id, ok := x.lookup(src, dst); ok {
		return id, nil
	}
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	e.mu.Lock()
	x, now := e.idx, e.idxAt
	e.mu.Unlock()
	// Someone refreshed while this waited: that is this miss's refresh.
	if now.Equal(at) && time.Since(now) >= indexRefreshMin {
		nets, err := e.nets.PrefixNetworks(ctx, e.names.netPrefix)
		if err != nil {
			return 0, fmt.Errorf("list the egress networks: %w", err)
		}
		e.setIndex(nets)
		e.mu.Lock()
		x = e.idx
		e.mu.Unlock()
	}
	if id, ok := x.lookup(src, dst); ok {
		return id, nil
	}
	return 0, ErrUnknownSource
}

// serveConn serves one connection the sidecar handed to the link listener:
// the app's TLS stream, unopened. Krill identifies the app, reads the SNI
// itself and lets the connection through only when the session is up, the
// app's organization is granted and (SNI, port) is both in the app's saved
// selection and on the relay's list; anything else is reset, and never
// reaches the destination by another way.
func (e *Egress) serveConn(conn net.Conn, hdr ProxyHeader, release func()) {
	defer conn.Close()
	appID, err := e.identify(e.ctx, hdr.Src.Addr(), hdr.Dst.Addr())
	if err != nil {
		release()
		slog.Warn("link: egress refused", "reason", err.Error(), "src", hdr.Src.Addr(), "dst", hdr.Dst.Addr())
		resetConn(conn)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(egressHelloTimeout))
	hello, sni, err := ReadClientHello(conn)
	release()
	if err != nil {
		slog.Warn("link: egress refused, no TLS ClientHello with a server name", "app", appID)
		resetConn(conn)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	d := Destination{Host: sni, Port: int(hdr.Dst.Port())}
	cell := e.traffic.cell(trafficKey{Service: appID, Dest: d.String()})
	granted, selected, err := e.store.egressDecision(e.ctx, appID, d)
	if err != nil {
		cell.refused.Add(1)
		slog.Error("link: egress: read the app's grant and selection failed", "app", appID, "err", err)
		resetConn(conn)
		return
	}
	client := e.svc.client
	f := egressFacts{session: e.svc.Role() == RoleClient, granted: granted, selected: selected, relay: client.relayAllows(d)}
	if f.session {
		f.session = client.Status().State == StateConnected
	}
	if err := decideEgress(f); err != nil {
		cell.refused.Add(1)
		slog.Warn("link: egress refused", "app", appID, "dest", d.String(), "reason", err.Error())
		resetConn(conn)
		return
	}
	octx, cancel := context.WithTimeout(e.ctx, HeaderTimeout)
	st, err := client.OpenEgress(octx, d, appID)
	cancel()
	if err != nil {
		cell.refused.Add(1)
		slog.Warn("link: egress refused, no stream through the link", "app", appID, "dest", d.String(), "category", category(err))
		resetConn(conn)
		return
	}
	if _, err := st.Write(hello); err != nil {
		cell.refused.Add(1)
		_ = st.Close()
		resetConn(conn)
		return
	}
	cell.up.Add(int64(len(hello)))
	cell.conns.Add(1)
	splice(conn, st, &cell.up, &cell.down)
}

// resetConn closes c with a TCP reset where it can: a refused connection
// ends at once, and the app sees it fail rather than hang.
func resetConn(c net.Conn) {
	raw := c
	if tc, ok := c.(*tls.Conn); ok {
		raw = tc.NetConn()
	}
	if tcp, ok := raw.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = raw.Close()
	_ = c.Close()
}

// Routes are the destinations the sidecar routes to Krill: every saved
// destination of an app in a granted organization, while the installation is
// a client. Anything else gets no route (the link's state, not the apps').
func (e *Egress) Routes(ctx context.Context) ([]Destination, error) {
	if e.svc.Role() != RoleClient {
		return nil, nil
	}
	apps, err := e.store.egressApps(ctx)
	if err != nil {
		return nil, err
	}
	var out []Destination
	for _, a := range apps {
		if a.Granted {
			out = append(out, a.Saved...)
		}
	}
	return SortDestinations(out), nil
}

// DynamicConfig is the sidecar's dynamic configuration (EgressDynamicConfig
// over Routes), empty while there is nothing to route.
func (e *Egress) DynamicConfig(ctx context.Context) (map[string]any, error) {
	routes, err := e.Routes(ctx)
	if err != nil {
		return nil, err
	}
	if len(routes) == 0 || e.upErr != nil {
		return map[string]any{}, nil
	}
	local, err := e.store.Local(ctx)
	if err != nil {
		// No certificates yet (the role was never on): nothing to route.
		return map[string]any{}, nil
	}
	return EgressDynamicConfig(routes, e.upstream, local), nil
}

// publishManifest sends the relay the client's services: every app of a
// granted organization with a saved selection.
func (e *Egress) publishManifest(ctx context.Context) {
	if e.svc.Role() != RoleClient {
		return
	}
	apps, err := e.store.egressApps(ctx)
	if err != nil {
		slog.Warn("link: build the manifest failed", "err", err)
		return
	}
	e.svc.client.SetManifest(manifestOf(apps))
}

// manifestOf lists the apps of granted organizations, by id.
func manifestOf(apps map[int64]*EgressApp) []ManifestService {
	var out []ManifestService
	for _, a := range apps {
		if !a.Granted || len(a.Saved) == 0 {
			continue
		}
		m := ManifestService{ID: a.ID, Path: a.Path}
		for _, d := range a.Saved {
			m.Destinations = append(m.Destinations, d.String())
		}
		out = append(out, m)
	}
	sortManifest(out)
	return out
}
