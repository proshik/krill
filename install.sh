#!/bin/sh
# Krill installer — host binary + systemd, Dokploy-style one-liner.
#
#   curl -sSL https://raw.githubusercontent.com/proshik/krill/master/install.sh | sudo sh
#
# What it does (idempotent — re-run to upgrade):
#   1. Verify root + Linux + a supported CPU arch (amd64/arm64).
#   2. Install Docker (official convenience script) if missing.
#   3. Initialize a single-node Docker Swarm if not already a manager.
#   4. Run a loopback-only Postgres container for Krill's own state.
#   5. Download the krill binary from the GitHub Release (checksum-verified).
#   6. Generate /etc/krill/krill.env (secrets created once, preserved on re-run).
#   7. Install + start a systemd service.
#   8. Print the admin URL and one-time credentials.
#
# Environment overrides (all optional):
#   KRILL_REPO            owner/repo to pull releases from   (default proshik/krill)
#   KRILL_VERSION         release tag to install, e.g. v0.1.0 (default latest)
#   KRILL_BINARY          path to a krill binary already on the host (skips the
#                         download — useful when the repo/release is private)
#   KRILL_DOMAIN          base domain (sets KRILL_BASE_DOMAIN)
#   KRILL_ACME_EMAIL      Let's Encrypt contact email
#   KRILL_ADVERTISE_ADDR  Swarm advertise address (default: auto-detected IP)
set -eu

KRILL_REPO="${KRILL_REPO:-proshik/krill}"
KRILL_VERSION="${KRILL_VERSION:-latest}"
ENV_FILE="/etc/krill/krill.env"
BIN_PATH="/usr/local/bin/krill"
UNIT_PATH="/etc/systemd/system/krill.service"
PG_CONTAINER="krill-postgres"
PG_IMAGE="postgres:17-alpine"

info() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m warn:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

need_cmd() { command -v "$1" >/dev/null 2>&1; }

# rand_hex N — N bytes of randomness as a lowercase hex string.
rand_hex() {
	n="$1"
	if need_cmd openssl; then
		openssl rand -hex "$n"
	else
		head -c "$n" /dev/urandom | od -An -tx1 | tr -d ' \n'
	fi
}

# --- 1. Preconditions --------------------------------------------------------
[ "$(id -u)" = "0" ] || die "must run as root (pipe to 'sudo sh')"
[ "$(uname -s)" = "Linux" ] || die "Krill installs on Linux only"

case "$(uname -m)" in
	x86_64 | amd64) ARCH="amd64" ;;
	aarch64 | arm64) ARCH="arm64" ;;
	*) die "unsupported architecture: $(uname -m) (need x86_64 or aarch64)" ;;
esac

need_cmd curl || die "curl is required"

UPGRADE="no"
[ -f "$ENV_FILE" ] && UPGRADE="yes"
if [ "$UPGRADE" = "yes" ]; then
	info "Existing install detected ($ENV_FILE) — upgrading (state and secrets preserved)."
else
	info "Fresh install."
fi

# --- 2. Docker ---------------------------------------------------------------
if ! need_cmd docker; then
	info "Docker not found — installing via get.docker.com ..."
	curl -fsSL https://get.docker.com | sh || die "Docker install failed"
fi
docker info >/dev/null 2>&1 || die "Docker daemon is not running"

# --- 3. Swarm ----------------------------------------------------------------
if [ "$(docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null)" = "active" ] &&
	[ "$(docker info --format '{{.Swarm.ControlAvailable}}' 2>/dev/null)" = "true" ]; then
	info "Swarm already active (this node is a manager)."
else
	ADVERTISE="${KRILL_ADVERTISE_ADDR:-}"
	if [ -z "$ADVERTISE" ]; then
		ADVERTISE="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"
		[ -z "$ADVERTISE" ] && ADVERTISE="$(hostname -I 2>/dev/null | awk '{print $1}')"
	fi
	[ -n "$ADVERTISE" ] || die "could not detect an advertise address; set KRILL_ADVERTISE_ADDR"
	info "Initializing Swarm (advertise-addr $ADVERTISE) ..."
	docker swarm init --advertise-addr "$ADVERTISE" >/dev/null || die "swarm init failed"
fi

# --- 4. State Postgres -------------------------------------------------------
# The password lives in krill.env; on a fresh install we generate it, on upgrade
# we reuse what's already configured so the existing volume stays usable.
if [ "$UPGRADE" = "yes" ]; then
	PG_PW="$(sed -n 's#^KRILL_DATABASE_URL=postgres://krill:\([^@]*\)@.*#\1#p' "$ENV_FILE")"
	[ -n "$PG_PW" ] || die "could not read existing Postgres password from $ENV_FILE"
else
	PG_PW="$(rand_hex 16)"
fi

if [ "$(docker inspect -f '{{.State.Running}}' "$PG_CONTAINER" 2>/dev/null)" = "true" ]; then
	info "Postgres container '$PG_CONTAINER' already running."
else
	# Remove a stopped leftover so the run below doesn't clash on the name.
	docker rm -f "$PG_CONTAINER" >/dev/null 2>&1 || true
	info "Starting Postgres ($PG_IMAGE, loopback-only) ..."
	docker run -d --name "$PG_CONTAINER" --restart unless-stopped \
		-e POSTGRES_USER=krill -e POSTGRES_PASSWORD="$PG_PW" -e POSTGRES_DB=krill \
		-v krill-pg-data:/var/lib/postgresql/data \
		-p 127.0.0.1:5432:5432 "$PG_IMAGE" >/dev/null || die "failed to start Postgres"
fi

info "Waiting for Postgres to accept connections ..."
i=0
until docker exec "$PG_CONTAINER" pg_isready -U krill -q >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -ge 60 ] && die "Postgres did not become ready in time"
	sleep 1
done

# --- 5. Binary ---------------------------------------------------------------
if [ -n "${KRILL_BINARY:-}" ]; then
	# Use a binary already present on this host (e.g. scp'd in) — skip download.
	[ -f "$KRILL_BINARY" ] || die "KRILL_BINARY=$KRILL_BINARY not found"
	info "Installing krill from local binary $KRILL_BINARY ..."
	install -m 0755 "$KRILL_BINARY" "$BIN_PATH"
else
	if [ "$KRILL_VERSION" = "latest" ]; then
		BASE_URL="https://github.com/$KRILL_REPO/releases/latest/download"
	else
		BASE_URL="https://github.com/$KRILL_REPO/releases/download/$KRILL_VERSION"
	fi
	ASSET="krill-linux-$ARCH"
	TMP="$(mktemp -d)"
	trap 'rm -rf "$TMP"' EXIT

	info "Downloading $ASSET ($KRILL_VERSION) ..."
	curl -fsSL "$BASE_URL/$ASSET" -o "$TMP/krill" || die "failed to download $ASSET"

	if curl -fsSL "$BASE_URL/checksums.txt" -o "$TMP/checksums.txt" 2>/dev/null; then
		want="$(awk -v a="$ASSET" '$2==a {print $1}' "$TMP/checksums.txt")"
		if [ -n "$want" ]; then
			if need_cmd sha256sum; then
				got="$(sha256sum "$TMP/krill" | awk '{print $1}')"
			else
				got="$(shasum -a 256 "$TMP/krill" | awk '{print $1}')"
			fi
			[ "$want" = "$got" ] || die "checksum mismatch for $ASSET (expected $want, got $got)"
			info "Checksum verified."
		else
			warn "checksums.txt has no entry for $ASSET — skipping verification"
		fi
	else
		warn "checksums.txt not found in release — skipping verification"
	fi

	install -m 0755 "$TMP/krill" "$BIN_PATH"
fi

# --- 6. Config ---------------------------------------------------------------
mkdir -p /etc/krill
if [ "$UPGRADE" = "yes" ]; then
	info "Keeping existing $ENV_FILE (secrets unchanged)."
	ADMIN_EMAIL="$(sed -n 's#^KRILL_ADMIN_EMAIL=##p' "$ENV_FILE")"
else
	ADMIN_EMAIL="admin@krill.local"
	[ -n "${KRILL_DOMAIN:-}" ] && ADMIN_EMAIL="admin@${KRILL_DOMAIN}"
	ADMIN_PW="$(rand_hex 12)"
	SECRET_KEY="$(rand_hex 32)"
	umask 077
	{
		echo "KRILL_DATABASE_URL=postgres://krill:${PG_PW}@127.0.0.1:5432/krill?sslmode=disable"
		echo "KRILL_ADMIN_EMAIL=${ADMIN_EMAIL}"
		echo "KRILL_ADMIN_PASSWORD=${ADMIN_PW}"
		echo "KRILL_SECRET_KEY=${SECRET_KEY}"
		[ -n "${KRILL_DOMAIN:-}" ] && echo "KRILL_BASE_DOMAIN=${KRILL_DOMAIN}"
		[ -n "${KRILL_ACME_EMAIL:-}" ] && echo "KRILL_ACME_EMAIL=${KRILL_ACME_EMAIL}"
		echo "KRILL_LISTEN_ADDR=:8080"
		# First boot is over plain http://<ip>:8080; secure cookies would not be
		# sent over HTTP and would break login. Flip to true once behind HTTPS.
		echo "KRILL_COOKIE_SECURE=false"
	} >"$ENV_FILE"
	chmod 0600 "$ENV_FILE"
fi

# --- 7. systemd unit ---------------------------------------------------------
cat >"$UNIT_PATH" <<EOF
[Unit]
Description=Krill control plane
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
Type=simple
User=root
EnvironmentFile=${ENV_FILE}
ExecStart=${BIN_PATH}
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

# --- 8. Start ----------------------------------------------------------------
info "Starting krill service ..."
systemctl daemon-reload
systemctl enable krill >/dev/null 2>&1 || true
# restart (not just enable --now) so an upgrade actually picks up the new binary.
systemctl restart krill

# --- Summary -----------------------------------------------------------------
IP="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"
[ -z "$IP" ] && IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
echo
info "Krill is running."
echo "  URL:    http://${IP:-<server-ip>}:8080"
echo "  Login:  ${ADMIN_EMAIL}"
if [ "$UPGRADE" = "yes" ]; then
	echo "  Password unchanged (see ${ENV_FILE})."
else
	echo "  Password: ${ADMIN_PW}    <-- shown once; stored in ${ENV_FILE}"
fi
echo
echo "Next steps:"
echo "  1. Point an A record at this server (${IP:-<server-ip>})."
echo "  2. Set the base domain and enable HTTPS, then set KRILL_COOKIE_SECURE=true"
echo "     in ${ENV_FILE} and run: systemctl restart krill"
echo
echo "Logs:    journalctl -u krill -f"
echo "Upgrade: re-run this installer."
