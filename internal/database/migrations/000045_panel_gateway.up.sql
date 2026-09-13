-- The Krill UI's own route through the Traefik gateway (internal/panel). One
-- row for the whole instance: the panel is global infrastructure, not an
-- organization's resource.
--
-- secret is the random value both gateway tokens are derived from (encrypted
-- with KRILL_SECRET_KEY when it is set). It is written by the control plane on
-- its first start, not here, so it comes from crypto/rand.
--
-- state walks off -> pending -> active. pending is routed but unproven; only a
-- request that reached the panel through the domain over HTTPS makes it active.
--
-- direct_port_closed records that the operator closed the UI port
-- (KRILL_LISTEN_ADDR) to the outside world under the firewall lockdown, leaving
-- it reachable only from the gateway. direct_port_close_pending is a close that
-- was applied with the dead-man switch armed and is still waiting for its
-- confirmation through the domain.
CREATE TABLE panel_gateway (
    id                        SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    secret                    TEXT NOT NULL,
    host                      TEXT NOT NULL DEFAULT '',
    state                     TEXT NOT NULL DEFAULT 'off' CHECK (state IN ('off', 'pending', 'active')),
    allowed_ips               TEXT NOT NULL DEFAULT '',
    direct_port_closed        BOOLEAN NOT NULL DEFAULT false,
    direct_port_close_pending BOOLEAN NOT NULL DEFAULT false,
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT panel_gateway_host_chk CHECK (state = 'off' OR host <> ''),
    -- Closing the direct port without a working domain is a lockout.
    CONSTRAINT panel_gateway_direct_chk CHECK (state = 'active' OR NOT (direct_port_closed OR direct_port_close_pending))
);
