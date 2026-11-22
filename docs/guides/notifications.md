# Notifications

Krill sends Telegram messages about what happens to an organization's apps, databases and
nodes: failures as they happen, successful deploys, disks filling up, memory limits that no
longer fit the node, and one digest a day. Everything is set up on **Settings → Notifications**
of each organization.

- [1. Setting up a channel](#1-setting-up-a-channel)
- [2. Events](#2-events)
- [3. Daily digest](#3-daily-digest)
- [4. Infrastructure alerts](#4-infrastructure-alerts)
- [5. What the numbers mean](#5-what-the-numbers-mean)
- [6. Security](#6-security)
- [7. Telegram through a proxy](#7-telegram-through-a-proxy)
- [8. Troubleshooting](#8-troubleshooting)

## 1. Setting up a channel

1. Create a bot with [@BotFather](https://t.me/BotFather) and copy its token.
2. Add the bot to the chat that should get the messages (or start a private chat with it), and
   find the chat's ID — for example by sending the bot a message and reading
   `https://api.telegram.org/bot<token>/getUpdates`.
3. On **Settings → Notifications** enter the token and the chat ID, tick **Enabled**, pick the
   **Message language** (English or Russian) and save.
4. **Send test message** checks the token and the chat; **Send digest now** sends today's digest
   right away, whatever its schedule.

The token is encrypted at rest (with `KRILL_SECRET_KEY`), never shown back and never logged.
Leaving the token field empty keeps the stored one.

## 2. Events

| Setting | Sent when | When |
|---------|-----------|------|
| Deploy failures | A deploy fails, or a database server's move to another node fails. | At once |
| Successful deploys | A deploy finishes with the app running. Deploys Krill makes itself (moving apps to their organization's network after an upgrade) are not reported. | At once |
| Backup failures | A database, volume or bucket backup (or restore) fails. | At once |
| App or database server down / recovered | An app or a database server that was running loses its tasks — a crash, or its node switched off — and when it comes back. Stopping a server, or moving it to another node, is not reported. | Down: after two failed health checks in a row (`KRILL_HEALTH_POLL_INTERVAL`, 30 s by default). Recovered: at the first healthy check after a down alert. |

A message about an app names the app on its own line, then its project and environment, then —
for a successful deploy — the version that went out, how long it took and what started it:

```
🟢 Deploy succeeded
demo-app
project side · environment prod
version v1.8.3 · 52s · via API
```

The app's name is a link to its page in Krill when the installation has a public https address:
`KRILL_PUBLIC_URL` (when it is `https://…`) or an active [panel domain](domains.md). Without one
the name is plain text — an address derived from `KRILL_HOST` alone is not linked.

The **version** is what the deploy shipped:

- an image app: the tag it deployed (`v1.8.3`). A tag that can be moved to another image by a
  later push — anything but a full `major.minor.patch` version or a commit hash, so `latest`,
  `main`, `stable-2`, `v1` or `16` — also gets the short digest of the image it pointed at,
  `latest (b1139b7c55e2)`, so two deploys of the same tag can be told apart;
- a Dockerfile app: the branch and the commit that was built, `master@1a09ec0`;
- a rollback: the version of the deploy it returned to.

Deploys made by v0.11.0 or earlier recorded no version; for them the message names the image instead.

Successful backups have no message of their own: they usually run at night, so they are listed in
the [daily digest](#3-daily-digest) instead. A failed restore is alerted like a failed backup but
is not counted among the digest's backup runs.

## 3. Daily digest

With **Send a daily digest** on, the channel gets one message a day at the chosen **Time** in the
chosen **Time zone** (11:00 Europe/Moscow by default; any IANA name such as `Europe/Amsterdam`
works). It covers the last 24 hours:

- **Needs attention** — first, so it is the first thing you see; or 🟢 **All good** when every
  figure below is green. It lists every yellow and red figure, red first, and its heading carries
  the worst of them.
Deploys, backups and apps are grouped under a bold **project · environment** heading, and app
names link to their pages as in the messages above.

- **Deploys**: one line per app — how many deploys succeeded, and the version of the newest
  successful deploy and when it went out (`🟢 demo-app — 5 deploys, now v1.8.3 (22:43)`); "now"
  is that deploy on record, not a check of the running service — an app stopped since then is
  listed under **Apps** as stopped. A deploy Krill made itself (moving apps to their
  organization's network) is not counted, but when it is the newest success its version is the one
  shown. Every failed deploy gets a 🔴 line of its own with its version, time and reason, however
  many there are; at most 20 success lines are listed, the rest counted. "none" when there were
  none.
- **Backups**: every database, volume and bucket backup run, named by what was backed up (the
  logical database, `app / volume`, the bucket), 🟢 with its stored size or 🔴 with the error;
  "none" when there were none. Runs journalled by v0.11.0 or earlier named a database by its server.
- **Nodes** (infrastructure channels only): one line per node with its figures in short — the
  fullest disk, the memory the containers use and the sum of their limits against the node, and
  CPU (the day's average and peak). The line carries the worst of its figures; when that is yellow
  or red, the next line says what made it so — the figures at that colour, at most two and "and N
  more" — so a red node is not read as its disk's. The full lines of every node follow in one
  collapsed quote (tap to expand), where a figure's own line is marked only when it is yellow or
  red:

  ```
  🔴 krill-penpot-msk — disk 10% · memory 36%, limits 107% · CPU 1% (peak 3%)
  ↳ memory limits at 107% of its memory
  ▸ krill-penpot-msk
    disk /: 10% used (12.9 of 148 GiB), 128 GiB free
    🔴 memory: containers use 2.6 of 7.3 GiB · limits add up to 7.8 GiB — 107% of the node’s memory
    CPU: 1% on average, 3% at peak — of all cores (2)
  ```

  An app is one line and every figure that colours it is on that line, so its mark has no
  separate reason.
- **Apps**: memory now, the day's peak against the limit, and crashes (only when there were
  any), each app marked with its status. In each project and environment the apps without fresh
  data follow, named on one line per what Swarm says: 🔴 **Not running** (it should
  run and does not), ⚪ **Stopped or never deployed** (zero replicas on purpose, or no service),
  ⚪ **No fresh data** (Swarm runs it, but its node's metrics are missing — or Swarm could not be
  read, and then every such app is listed here).

| Figure | 🟡 from | 🔴 from |
|--------|---------|---------|
| Disk use | the channel's disk threshold − 15 points (70 % by default) | the disk threshold (85 %) |
| Sum of memory limits against the node | 80 % (or the threshold, if lower) | the channel's memory-limits threshold (100 %) |
| Memory the containers use on the node | 80 % | 90 % |
| Node CPU, the day's average | 70 % | 90 % |
| No disk data for a node that reports | always | — |
| A node that stopped reporting | — | always |
| An app's memory peak against its limit | 80 % | 95 % |
| An app crashed (see [Crashes](#5-what-the-numbers-mean)) | — | once |
| Failed deploy or backup, app not running | — | always |

The CPU peak is shown but not graded: a peak is one sampling interval, and a build or a backup
reaches 100 % routinely.

A long digest is split into several messages, at line boundaries; when the split falls inside the
collapsed node details, the quote is closed at the end of one message and opened again in the
next. If Krill is not running at the scheduled time, the
digest is sent when it comes back, but only within 3 hours of that time; otherwise that day is
skipped, so a digest never arrives at night. The digest is sent once per local day — also across
restarts, and also when the time or zone is changed after today's digest went out — and a failed
send is retried every minute within those 3 hours. When a digest went out late, the next one lists
deploys and backups since that digest rather than for exactly 24 hours, so nothing falls between
the two (up to 48 hours back).

Backup runs are kept for the digest for 30 days.

## 4. Infrastructure alerts

The nodes are shared by every organization of an installation, so node data goes only to channels
an **instance operator** (the administrator Krill was installed with) has opted in. On those
channels the operator can set:

- **Node alerts and the node section of the digest** — the opt-in itself.
- **Disk usage alert, %** (85 by default, 50–99). A filesystem at or over it is reported at once,
  at any time of day; while it stays there it is reported again at most once a day, and once it
  is 3 points below the threshold a "back to normal" message follows.
- **Memory limits alert, % of node memory** (100 by default, 50–200). After every successful
  deploy Krill adds up the memory limits of the containers on the node the app landed on and
  warns when the sum reaches this share of the node's memory. The deploy itself is never
  blocked; the message names the deploy that prompted it only in a channel of the same
  organization — other organizations' channels learn about the node, not about the app.

Organization admins see these settings but cannot change them, and if one of them points the
channel at another bot or chat, node alerts are turned off until an operator turns them on again.

Messages about nodes name the installation (the host of its public URL), so two installations
reporting into one chat can be told apart.

## 5. What the numbers mean

- **Disk** is what `df` reports for `/` and for Docker's data directory (`/var/lib/docker`, or
  wherever the daemon keeps it) when that is a separate filesystem: used / (used + available), so
  blocks reserved for root count as full. "Free" is `df`'s *Avail* — what an unprivileged process can still
  write (a container running as root may also use the reserve) — so used + free can be less than
  the size by the root reserve (5 % on ext4 by default).
  A line reads `disk /: 10% used (12.9 of 148 GiB), 128 GiB free`; a unit both sizes share is
  written once. Krill takes the snapshot every 10 sampling intervals
  (about 5 minutes by default) — locally on the control-plane node and over the node's SSH
  connection on workers. The SSH user needs no special rights.
- **Memory** ("memory: containers use 2.6 of 7.3 GiB") and **CPU** add up the containers on the
  node, as the **Monitoring** page does: the operating system and the Krill process itself (a host
  binary on the control-plane node) are not included. CPU is a share of all the node's cores
  together ("— of all cores (2)").
- **Memory limits** ("limits add up to 7.8 GiB — 107% of the node's memory") count every
  container with a limit; over 100 % means the containers may ask for more than the node has. A
  container without one is counted apart ("containers without a limit"): it can use the whole
  node.
- An app's line reads `demo-app — 335 MiB, peak 584 MiB of 1.5 GiB (38%)`: the memory it uses
  at the time of the digest, and the day's peak of one container against its limit.
- **Crashes** are the app's failed tasks in Swarm's history over the day: a non-zero exit code
  (137 is a SIGKILL — an OOM kill, or a stop that outlived the grace period), a container killed
  for failing its healthcheck, a task Swarm could not start. Planned stops are not counted:
  - a deploy — the old task ends as *shutdown*;
  - an exit on SIGTERM, `non-zero exit (143)` — the Docker daemon stopping the container or a
    manual `docker stop`. A production worker showed exactly this for a JVM app after
    `systemctl restart docker`; an app that exits 0 on SIGTERM ends as *complete* and was never
    counted. A reboot of the node is expected to look the same, but has not been checked;
  - a container gone under its task, "No such container" — seen after a daemon restart on a test
    cluster.
- Only an app that exits within its stop grace period (10 s by default) on SIGTERM avoids being
  counted when Docker restarts; one that takes longer is SIGKILLed (137), and one with its own
  stop signal exits with another code (130 for SIGINT) — both count. A healthcheck kill also
  starts with SIGTERM, but Swarm adds "unhealthy container" to its error, and it counts; an app
  that exits 143 on its own is not told apart from a stop. Swarm keeps a few tasks per replica,
  so for an app that crash-loops the number is a lower bound.
- An app with no samples in the last 10 minutes is listed under **Not running**, **Stopped or
  never deployed** or **No fresh data**, by its Swarm service.
- **Backups** are read from a run journal that exists since v0.9.0: the first digest after the
  upgrade to it lists only the runs since the upgrade.

## 6. Security

- Messages never contain secret values: no tokens, passwords or connection strings.
- The digest of an organization lists only that organization's apps, deploys and backups.
- Node data reaches only the channels an instance operator opted in (see above).
- Telegram receives everything a message says; choose the chat accordingly.

## 7. Telegram through a proxy

Some networks cannot reach `api.telegram.org`: the name resolves to an address that never
answers, or Telegram is blocked. `KRILL_TELEGRAM_PROXY` sends Krill's Telegram messages — every
alert, the test message and the digest, for every organization — through a proxy, and nothing
else. Deploys, image pulls, update checks and backups keep going direct, so they never depend on
the proxy. (`HTTPS_PROXY` would route all of them; once `KRILL_TELEGRAM_PROXY` is set, it no
longer applies to Telegram.)

The value is a proxy URL:

| Form | Proxy |
|------|-------|
| `socks5h://127.0.0.1:1080` | SOCKS5; the proxy resolves `api.telegram.org` |
| `socks5://127.0.0.1:1080` | the same — Krill always hands the name to a SOCKS5 proxy |
| `http://proxy.example:3128` | an HTTP proxy, through `CONNECT` |
| `https://proxy.example:3128` | the same, with TLS to the proxy |

A user and password go in the URL (`socks5h://user:pass@host:1080`); Krill never logs or shows
them. Unset or empty means direct, as before. Each message still has 10 seconds in all.

Set it for the service with a systemd drop-in, then restart Krill:

```ini
# /etc/systemd/system/krill.service.d/20-telegram-proxy.conf
[Service]
Environment=KRILL_TELEGRAM_PROXY=socks5h://127.0.0.1:1080
```

```sh
sudo systemctl daemon-reload && sudo systemctl restart krill
```

At startup Krill logs `notify: Telegram messages go through a proxy proxy=socks5h://127.0.0.1:1080`.
A value it cannot use (a typo, a scheme other than the four above, a path) does not stop Krill:
it logs `notify: Telegram proxy setting rejected; Telegram messages will fail until it is fixed`
with the reason, and every message fails with `telegram: not sent: KRILL_TELEGRAM_PROXY is
invalid: …` — Krill never falls back to sending directly. **Send test message** then shows an
error, and the reason is in the log line `testNotification: send failed`.

**Settings → Notifications** shows the setting under the chat ID, read-only (it belongs to the
machine, not to an organization): "Telegram messages are sent via proxy socks5h://127.0.0.1:1080"
for an instance operator — scheme and address, never the credentials — and "…via a proxy set by
the installation's administrator" for everyone else. A rejected value shows in red with the
reason and "Messages are not sent"; with no proxy there is no line at all.

One way to get a proxy is an SSH tunnel to a host that reaches Telegram, kept up by its own
systemd unit: `ssh -N -D 127.0.0.1:1080 tunnel@other-host`, with the key restricted on the other
host to `permitopen="api.telegram.org:443"`. Krill does not run or watch the tunnel: while it is
down, messages fail with a connection error in the log.

## 8. Troubleshooting

- **No messages at all** — **Send test message**. An error there means a wrong token or chat ID,
  the bot was never added to the chat, or Telegram is out of reach (see
  [Telegram through a proxy](#7-telegram-through-a-proxy)). If the stored token cannot be
  decrypted (the `KRILL_SECRET_KEY` changed), re-enter it.
- **A digest or alert arrives, yet the log says it failed** — fixed in v0.9.4: before it, a reply
  from Telegram over 4 KiB (a long digest) was misread as a failure, and the scheduled digest was
  re-sent every minute for up to 3 hours. If Telegram accepts a message but its reply cannot be
  read, Krill now counts the message as sent and logs `notify: Telegram accepted a message (200)
  with a reply that could not be read`.
- **No digest** — check that **Enabled** and **Send a daily digest** are both on and that the time
  zone is what you expect; **Send digest now** shows what the channel would get. Krill logs
  `notify digest sent` (or `notify digest: send failed, will retry`) for every attempt.
- **Disk shows "no data"** — the node's snapshot failed: `df` is missing (Krill running inside a
  container) or the worker cannot be reached over SSH. Krill logs
  `metrics: node disk usage failed` with the reason.
