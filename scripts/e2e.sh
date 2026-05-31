#!/usr/bin/env bash
# Krill PHASE 0 — автоматический e2e прогон (Task 15).
set -uo pipefail
R=.
cd "$R"

export KRILL_LISTEN_ADDR=:8080
export KRILL_DATABASE_URL="postgres://krill:krill@localhost:5432/krill?sslmode=disable"
export KRILL_ADMIN_EMAIL="admin@krill.local"
export KRILL_ADMIN_PASSWORD="changeme"
export KRILL_DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
export KRILL_BASE_DOMAIN="127-0-0-1.sslip.io"
export KRILL_NETWORK="krill-net"
export KRILL_COOKIE_SECURE=false

APP=e2eweb
PG=$(docker ps --filter name=krill-postgres --format '{{.Names}}' | head -1)
JAR=/tmp/krill_cookies.txt
LOG=/tmp/krill_app.log
rm -f "$JAR"

echo "### 0. cleanup prior state"
docker service rm krill-$APP >/dev/null 2>&1 || true
[ -n "$PG" ] && docker exec "$PG" psql -U krill -d krill -c "DELETE FROM applications WHERE name='$APP'" >/dev/null 2>&1 || true

echo "### 1. start krill (background)"
./bin/krill > "$LOG" 2>&1 &
KPID=$!
echo "krill pid=$KPID"

cleanup() { kill "$KPID" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "### 2. wait for listen (max 20s)"
up=0
for i in $(seq 1 40); do
  if curl -fsS -o /dev/null "http://127.0.0.1:8080/login"; then up=1; break; fi
  sleep 0.5
done
echo "listening=$up"
if [ "$up" != 1 ]; then echo "--- app log ---"; cat "$LOG"; exit 1; fi

echo "### 3. traefik service (wait up to 30s)"
tr=0
for i in $(seq 1 15); do
  if docker service ls --filter name=krill-traefik --format '{{.Name}} {{.Replicas}}' | grep -q 1/1; then tr=1; break; fi
  sleep 2
done
docker service ls --filter name=krill-traefik --format 'TRAEFIK: {{.Name}} {{.Replicas}}'
echo "traefik_ready=$tr"

echo "### 4. login"
code=$(curl -s -o /dev/null -w '%{http_code}' -c "$JAR" \
  --data-urlencode "email=$KRILL_ADMIN_EMAIL" \
  --data-urlencode "password=$KRILL_ADMIN_PASSWORD" \
  "http://127.0.0.1:8080/login")
echo "login_http=$code (expect 303)"
echo "cookie_set=$(grep -c krill_session "$JAR")"

echo "### 5. protected access with cookie"
echo "apps_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" http://127.0.0.1:8080/apps) (expect 200)"

echo "### 6. create app from nginx:alpine"
code=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" \
  --data-urlencode "name=$APP" \
  --data-urlencode "image=nginx" \
  --data-urlencode "tag=alpine" \
  --data-urlencode "domain=" \
  --data-urlencode "port=80" \
  --data-urlencode "env=" \
  "http://127.0.0.1:8080/apps")
echo "create_http=$code (expect 303)"

ID=$(docker exec "$PG" psql -U krill -d krill -t -A -c "SELECT id FROM applications WHERE name='$APP'")
echo "app_id=$ID  domain=$(docker exec "$PG" psql -U krill -d krill -t -A -c "SELECT domain FROM applications WHERE name='$APP'")"

echo "### 7. deploy"
echo "deploy_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X POST \
  --data-urlencode "image=nginx" --data-urlencode "tag=alpine" \
  "http://127.0.0.1:8080/apps/$ID/deploy") (expect 303)"

echo "### 8. wait for swarm service running (max 90s)"
run=0
for i in $(seq 1 45); do
  rep=$(docker service ls --filter name=krill-$APP --format '{{.Replicas}}')
  if [ "$rep" = "1/1" ]; then run=1; break; fi
  sleep 2
done
docker service ls --filter name=krill-$APP --format 'SERVICE: {{.Name}} {{.Image}} {{.Replicas}}'
echo "service_running=$run"
docker service ps krill-$APP --format 'TASK: {{.Name}} {{.CurrentState}}' 2>/dev/null | head -3

echo "### 9. status endpoint (UI badge)"
echo "status_badge: $(curl -s -b "$JAR" "http://127.0.0.1:8080/apps/$ID/status")"

echo "### 10. reach app through Traefik (host-mode :80)"
DOMAIN="$APP.127-0-0-1.sslip.io"
for attempt in 1 2 3 4 5; do
  out=$(curl -s -m 5 --resolve "$DOMAIN:80:127.0.0.1" -o /tmp/traefik_resp.txt -w '%{http_code}' "http://$DOMAIN/")
  [ "$out" = "200" ] && break
  sleep 2
done
echo "traefik_http=$out (expect 200)"
echo "traefik_body_is_nginx=$(grep -ci 'nginx\|Welcome' /tmp/traefik_resp.txt)"

echo "### 11. rolling-update: change tag alpine -> 1.27-alpine"
echo "redeploy_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X POST \
  --data-urlencode "image=nginx" --data-urlencode "tag=1.27-alpine" \
  "http://127.0.0.1:8080/apps/$ID/deploy") (expect 303)"
sleep 6
docker service ps krill-$APP --format 'PS: {{.Name}} {{.Image}} {{.CurrentState}}' 2>/dev/null | head -5
echo "service_image_now=$(docker service ls --filter name=krill-$APP --format '{{.Image}}')"

echo "### 12. app log tail"
echo "--- krill log (last 15) ---"
tail -15 "$LOG"

echo "### DONE"
