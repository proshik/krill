# Krill link

The Krill link connects two Krill installations. A **client** — an installation that cannot
reach some foreign APIs directly — keeps one persistent outbound TLS connection to a **relay**
abroad, and both sides carry streams over it. Telegram's Bot API and LLM APIs are the usual
examples: the relay reaches them, the client does not.

Apps on the client reach the hosts the relay allows **through the link, without a change to
their code or environment**: an app calls `https://api.telegram.org` (long polling included) or
`https://api.openai.com` by its usual name, and the connection leaves from the relay. TLS stays
end to end — the relay sees the host, the port and the sizes, never the content or a bot token.
Receiving webhooks through the relay (ingress) arrives in a later release.

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
- A client's certificate lasts a year and is renewed over the live connection when two thirds
  of that has passed; the client then reconnects at once with the new one (a second's break). The relay reissues its own server
  certificate under the same CA 90 days before it expires, with nothing to do on the clients.
  The link CA lasts ten years.

## Requirements

A **relay** needs what the [panel domain](domains.md#serve-the-krill-ui-on-a-domain) needs —
`KRILL_ADVERTISE_ADDR` set and Krill listening on a non-loopback address — because its gateway
forwards the link to Krill the same way. The panel domain itself can stay off. It also listens on
`KRILL_LINK_LISTEN_ADDR` (default `:8445`, see [configuration](../configuration.md#gateway)); only
the gateway connects there.

A **client** needs nothing open.

Both sides must run a Krill release with the link. They update independently; a relay refuses a
client whose link protocol version it does not speak, with a clear message on both pages.

`KRILL_LINK_LISTEN_ADDR` must not use a port the firewall lockdown keeps open to everyone (22,
80, 443 or the UI port): the role is refused with that reason, because the listener would then
be reachable from the world rather than from the gateway only.

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

On the relay, each client's row shows its session or when it was last seen, its certificate's
expiry and the latest refused session: a second one from another address (a clone restored
from a backup) or a protocol version the relay does not speak.

## Send an app's traffic through the link (egress)

Three people decide, in this order:

1. **The relay operator** keeps the relay's **destinations** (Settings → Link → Destinations):
   exact host names and ports, the same for every client — for example `api.telegram.org:443`
   and `api.openai.com:443`. This list is the hard boundary: the relay refuses any other
   destination whatever a client asks.
2. **The client operator** grants the link to an organization (Settings → Link →
   Organizations).
3. **An admin of that organization** picks, on the app's **Settings** tab, which of the
   relay's destinations the app sends through the link, saves, and deploys (or **Apply
   settings**). Until then the app shows its settings as not applied.

From the next deploy on, the app resolves the chosen hosts to Krill's egress sidecar, and its
connections to them go through the link:

```
app ─ api.telegram.org:443 ─▶ krill-link-egress (on the app's own network)
                               │ SNI, unopened, PROXY v2 + mTLS
                               ▼
                             Krill (client): which app? granted? chosen? on the relay's list?
                               │ egress stream over the link
                               ▼
                             Krill (relay): on its list? SNI = host? resolve, no private address
                               │
                               ▼
                             api.telegram.org:443
```

Example: a Telegram bot with long polling and an app calling an LLM API.

1. Relay: add `api.telegram.org` port `443` and `api.openai.com` port `443` to the
   destinations.
2. Client: grant the bot's organization.
3. Bot app → Settings → **Egress through the link**: tick `api.telegram.org:443`, save,
   deploy. The bot's `getUpdates` and `sendMessage` now leave from the relay; nothing in the
   bot changes. The LLM app: tick `api.openai.com:443` the same way.

Both Link pages show each service with its destinations, whether the relay allows them, and
the bytes carried since Krill started. The relay lists a client's services under its card.

### What stays true when something fails

- **The link is down, a destination was removed from the relay's list or from the app, or the
  organization was revoked:** the app's new connections to those hosts are refused at once. They
  are never sent directly.
- **A destination removed from the app's selection** stays intercepted (and refused) until a
  deploy without it has finished; only then does the app resolve the real address again.
- **Turning the link off or revoking an organization** marks its apps' settings as not applied;
  their next deploy takes them off the link, and from then on they reach those hosts directly.

### Limits

- TLS only, exact host names: no wildcard, no IP, no UDP, no Telegram MTProto.
- Interception is per **host**, not per port: an app that picks `api.example.org:8443` also sends
  its connections to `api.example.org:443` to the link, where they are refused, never sent
  directly. The app's card says so.
- The **pre-deploy command** runs in a one-off container on the organization network and does
  not use the link.
- Each app with egress gets its own network (`krill-egress-<app id>`) and the sidecar sits on all
  of them; an app without egress keeps resolving real addresses. A change of an app's hosts
  restarts the sidecar's task (StartFirst), which can break open connections for a moment; long
  polling and SDKs reconnect.
- Granting an organization and saving an app's destinations are refused while the [rollback
  binary](#rollback) is older than the link core release (`v0.17.0`) or cannot be read: a
  rollback onto a release without the link core would send the apps' traffic directly. The
  pages say why and link to Settings → Updates.
- A client needs what a relay needs for its listener: `KRILL_ADVERTISE_ADDR` set and Krill
  listening on a non-loopback address, because the sidecar reaches Krill that way. The Link
  page says when that is missing.

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

The link core's deployer (`v0.17.0`) keeps every app's attachments to `krill-egress-*`
networks on a redeploy and its listener refuses egress streams, so rolling back one release from
the egress release fails closed: the apps' connections to their link hosts are refused until
Krill is updated again. Going further back does not: see [Rollback](../install.md#rollback).
That is why egress stays off while `krill.prev` (the binary **Roll back** and the automatic
revert put back) is older than the core release.
