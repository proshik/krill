#!/usr/bin/env bash
# Krill PHASE 0 — автоматический e2e прогон (Task 15).
# Полностью изолирован: свой Postgres на 55432, Krill на 18080, Traefik на host :80.
# Не зависит от занятых на машине портов 5432/8080.
set -uo pipefail
R=.
cd "$R"

HTTP_PORT=18080
PG_PORT=55432
PG_NAME=krill-e2e-pg
APP=e2eweb
JAR=/tmp/krill_cookies.txt
LOG=/tmp/krill_app.log
rm -f "$JAR"

export KRILL_LISTEN_ADDR=:$HTTP_PORT
export KRILL_DATABASE_URL="postgres://krill:krill@localhost:$PG_PORT/krill?sslmode=disable"
export KRILL_ADMIN_EMAIL="admin@krill.local"
export KRILL_ADMIN_PASSWORD="changeme"
export KRILL_DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
export KRILL_BASE_DOMAIN="127-0-0-1.sslip.io"
export KRILL_NETWORK="krill-net"
export KRILL_COOKIE_SECURE=false

cleanup() {
  kill "${KPID:-0}" >/dev/null 2>&1 || true
  docker rm -f "$PG_NAME" >/dev/null 2>&1 || true
  docker service rm krill-$APP >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "### 0. dedicated postgres on :$PG_PORT"
docker rm -f "$PG_NAME" >/dev/null 2>&1 || true
docker run -d --name "$PG_NAME" -e POSTGRES_USER=krill -e POSTGRES_PASSWORD=krill \
  -e POSTGRES_DB=krill -p $PG_PORT:5432 postgres:16-alpine >/dev/null
for i in $(seq 1 30); do
  docker exec "$PG_NAME" pg_isready -U krill >/dev/null 2>&1 && break
  sleep 1
done
echo "pg_ready=$(docker exec "$PG_NAME" pg_isready -U krill 2>&1 | grep -c 'accepting')"

echo "### 0b. clean prior swarm service"
docker service rm krill-$APP >/dev/null 2>&1 || true

echo "### 1. start krill on :$HTTP_PORT"
./bin/krill > "$LOG" 2>&1 &
KPID=$!
echo "krill pid=$KPID"

echo "### 2. wait for listen (max 25s)"
up=0
for i in $(seq 1 50); do
  curl -fsS -o /dev/null "http://127.0.0.1:$HTTP_PORT/login" && { up=1; break; }
  kill -0 "$KPID" 2>/dev/null || { echo "krill exited early"; break; }
  sleep 0.5
done
echo "listening=$up"
if [ "$up" != 1 ]; then echo "--- app log ---"; cat "$LOG"; exit 1; fi

echo "### 3. traefik service (wait up to 60s; first run pulls image)"
tr=0
for i in $(seq 1 30); do
  docker service ls --filter name=krill-traefik --format '{{.Replicas}}' | grep -q 1/1 && { tr=1; break; }
  sleep 2
done
docker service ls --filter name=krill-traefik --format 'TRAEFIK: {{.Name}} {{.Image}} {{.Replicas}}'
echo "traefik_ready=$tr"

echo "### 4. login"
echo "login_http=$(curl -s -o /dev/null -w '%{http_code}' -c "$JAR" \
  --data-urlencode "email=$KRILL_ADMIN_EMAIL" --data-urlencode "password=$KRILL_ADMIN_PASSWORD" \
  "http://127.0.0.1:$HTTP_PORT/login") (expect 303)"
echo "cookie_set=$(grep -c krill_session "$JAR")"

echo "### 5. protected access with cookie"
echo "apps_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" http://127.0.0.1:$HTTP_PORT/apps) (expect 200)"
echo "apps_http_nocookie=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$HTTP_PORT/apps) (expect 303 -> login)"

echo "### 6. create app from nginx:alpine"
echo "create_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" \
  --data-urlencode "name=$APP" --data-urlencode "image=nginx" --data-urlencode "tag=alpine" \
  --data-urlencode "domain=" --data-urlencode "port=80" --data-urlencode "env=" \
  "http://127.0.0.1:$HTTP_PORT/apps") (expect 303)"
ID=$(docker exec "$PG_NAME" psql -U krill -d krill -t -A -c "SELECT id FROM applications WHERE name='$APP'")
DOM=$(docker exec "$PG_NAME" psql -U krill -d krill -t -A -c "SELECT domain FROM applications WHERE name='$APP'")
echo "app_id=$ID domain=$DOM"

echo "### 7. deploy"
echo "deploy_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X POST \
  --data-urlencode "image=nginx" --data-urlencode "tag=alpine" \
  "http://127.0.0.1:$HTTP_PORT/apps/$ID/deploy") (expect 303)"

echo "### 8. wait for swarm service running (max 120s; pulls nginx)"
run=0
for i in $(seq 1 60); do
  [ "$(docker service ls --filter name=krill-$APP --format '{{.Replicas}}')" = "1/1" ] && { run=1; break; }
  sleep 2
done
docker service ls --filter name=krill-$APP --format 'SERVICE: {{.Name}} {{.Image}} {{.Replicas}}'
echo "service_running=$run"
docker service ps krill-$APP --format 'TASK: {{.CurrentState}}' 2>/dev/null | head -2

echo "### 9. status endpoint (UI badge)"
echo "status_badge: $(curl -s -b "$JAR" "http://127.0.0.1:$HTTP_PORT/apps/$ID/status")"

echo "### 10. reach app through Traefik (host :80)"
out=000
for attempt in $(seq 1 8); do
  out=$(curl -s -m 5 -H "Host: $DOM" -o /tmp/traefik_resp.txt -w '%{http_code}' "http://127.0.0.1:80/")
  [ "$out" = "200" ] && break
  sleep 2
done
echo "traefik_http=$out (expect 200)"
echo "traefik_body_is_nginx=$(grep -ci 'nginx\|Welcome' /tmp/traefik_resp.txt 2>/dev/null || echo 0)"

echo "### 11. rolling-update: tag alpine -> 1.27-alpine"
echo "redeploy_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X POST \
  --data-urlencode "image=nginx" --data-urlencode "tag=1.27-alpine" \
  "http://127.0.0.1:$HTTP_PORT/apps/$ID/deploy") (expect 303)"
for i in $(seq 1 30); do
  docker service ls --filter name=krill-$APP --format '{{.Image}}' | grep -q '1.27' && break
  sleep 2
done
echo "service_image_now=$(docker service ls --filter name=krill-$APP --format '{{.Image}}')"
docker service ps krill-$APP --format 'PS: {{.Image}} {{.CurrentState}}' 2>/dev/null | head -4
out2=$(curl -s -m 5 -H "Host: $DOM" -o /dev/null -w '%{http_code}' "http://127.0.0.1:80/")
echo "traefik_after_update_http=$out2 (expect 200, no downtime)"

echo "### 12. krill log tail"
tail -12 "$LOG"
echo "### DONE"
