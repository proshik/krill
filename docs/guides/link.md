# Krill link

The Krill link connects two Krill installations. A **client** — an installation that cannot
reach some foreign APIs directly — keeps one persistent outbound TLS connection to a **relay**
abroad, and both sides carry streams over it. Telegram's Bot API and LLM APIs are the usual
examples: the relay reaches them, the client does not.

This release builds the link itself: roles, pairing, the encrypted transport, reconnection, the
relay's endpoint behind its gateway and the operator page. **It does not route any app traffic
yet**: sending apps' outbound connections through the link (egress) and receiving webhooks
through the relay (ingress) arrive in later releases. Setting the link up now lets you check that
the connection holds on your real network before anything depends on it.

## How it works

```
client                                         relay
Krill ── one TCP connection, TLS 1.3 ─────────▶ Traefik :443  HostSNI(link host), passthrough
          (mutual certificates,                    │  PROXY v2 + the gateway's certificate
           multiplexed streams)                    ▼
                                                Krill link listener (KRILL_LINK_LISTEN_ADDR)
```

- The relay has its own private **link CA**. It issues the relay's server certificate for the
  **link host** and a client certificate to every client it pairs. Only that CA is trusted; no
  public certificate is involved.
- The client opens no port. It dials the relay, verifies it against the CA fingerprint from the
  pairing code, and reconnects on its own (after 1 s, doubling up to 60 s) when the connection
  drops.
- The relay checks a client's certificate against its list of paired clients on every
  connection, so removing a client cuts it off at once. A client has one session at a time: a
  second installation with the same identity (a copy restored from a [state
  backup](krill-backups.md)) is refused and shown as a duplicate.
- Certificates last a year and are renewed over the live connection when two thirds of that has
  passed.

## Requirements

A **relay** needs what the [panel domain](domains.md#serve-the-krill-ui-on-a-domain) needs —
`KRILL_ADVERTISE_ADDR` set and Krill listening on a non-loopback address — because its gateway
forwards the link to Krill the same way. The panel domain itself can stay off. It also listens on
`KRILL_LINK_LISTEN_ADDR` (default `:8445`, see [configuration](../configuration.md#gateway)); only
the gateway connects there.

A **client** needs nothing open.

Both sides must run a Krill release with the link. They update independently; a relay refuses a
client whose link protocol version it does not speak, with a clear message on both pages.

## Set up the relay

1. Pick a **link host**: a name no app and no panel domain uses, for example
   `link.relay.example.com`. A DNS record pointing at the relay is recommended so the connection
   looks ordinary, but it is not required — clients dial the address in the pairing code.
2. On the relay, open **Settings → Link**, enter the link host under **Make this Krill a
   relay** and confirm. Krill creates the link CA and a certificate for the host, and the
   gateway starts routing it within a few seconds.
3. If the firewall is locked down (Settings → Firewall), **re-apply it**: the lockdown predates
   the link and keeps its port closed to the gateway. The Link page says so until you do. Krill
   never changes the firewall by itself.

The link host is reserved like the panel domain: no app can take it, and it cannot be set while
an app domain or the panel domain uses it.

## Pair a client

1. On the relay, under **New pairing code**, enter a short name for the client (`msk`) and,
   unless the client reaches the relay at `<link host>:443`, the `host:port` it dials (for
   example `203.0.113.10:443`). Create the code and copy it — it is shown once, works once and
   expires in 10 minutes.
2. On the client, open **Settings → Link**, choose **Make this Krill a client**, paste the code
   under **Pair with a relay** and submit. The client creates its key, connects to the relay
   and receives its certificate.
3. Both pages now show the session: connected since, latency, the other side's Krill version
   and the certificate's expiry.

Treat a pairing code like a password while it is valid: whoever redeems it first becomes that
client. Pairing attempts are limited to 10 per minute per address.

## States and errors

The client's page shows its state and the last error by category:

| Category | Meaning |
|---|---|
| `dns` | the relay's address does not resolve |
| `tcp` | the relay cannot be reached, or the connection dropped |
| `tls` | the relay did not prove the pinned CA, or TLS failed (a DPI box in between, an expired certificate) |
| `unauthorized` | the relay does not know this client: it was removed there, or paired again elsewhere |
| `incompatible` | the two releases speak different link protocol versions: update one side |
| `duplicate` | another installation holds this client's session |

If the relay's link host is also held by a domain — possible after a rollback to a release
without the link — the relay does not publish the link route and its page lists both records.
Remove the domain or pick another link host.

## Remove a client, forget the relay

- **Remove client** on the relay ends its session and invalidates its certificate. Pairing it
  again needs a new code.
- **Forget relay** on the client erases its key and certificate. Ask the relay operator to
  remove the client too.
- **Turn the link off** on either side closes the listener and the sessions but keeps the
  pairing and the CA, so turning it back on needs no new codes.

## Rollback

A relay rolled back to a release without the link stops routing the link host; clients keep
retrying and reconnect once the relay is back on a release with it.

The link core's deployer keeps every app's attachments to `krill-egress-*` networks on a
redeploy, so rolling back one release from the egress release fails closed. Going further back
does not: see [Rollback](../install.md#rollback).
