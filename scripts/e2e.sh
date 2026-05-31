#!/usr/bin/env bash
# Krill PHASE 1 — автоматический e2e прогон (Task 14).
# Полностью изолирован: свой Postgres на 55432, Krill на 18080, Traefik на host :80.
# Не зависит от занятых на машине портов 5432/8080.
# Флоу: login → resolve default org → project → environment → app → deploy →
#       Traefik-роутинг → save env → rolling-update (tag 1.27-alpine).
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
  # Имя сервиса приложения теперь krill-<appID> и неизвестно на момент trap'а —
  # снимаем все krill-* сервисы кроме traefik, чтобы не утёк e2e-app.
  docker service ls --filter name=krill- -q 2>/dev/null \
    | xargs -r -I{} sh -c 'n=$(docker service inspect --format "{{.Spec.Name}}" {} 2>/dev/null); [ "$n" = "krill-traefik" ] || docker service rm {} >/dev/null 2>&1' || true
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

echo "### 0b. clean prior app swarm services (by id, not traefik)"
docker service ls --filter name=krill- -q 2>/dev/null \
  | xargs -r -I{} sh -c 'n=$(docker service inspect --format "{{.Spec.Name}}" {} 2>/dev/null); [ "$n" = "krill-traefik" ] || docker service rm {} >/dev/null 2>&1' || true

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
echo "orgs_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" http://127.0.0.1:$HTTP_PORT/orgs) (expect 200)"
echo "orgs_http_nocookie=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$HTTP_PORT/orgs) (expect 303 -> login)"

echo "### 6. resolve default org id"
ORG=$(docker exec "$PG_NAME" psql -U krill -d krill -t -A -c "SELECT id FROM organizations WHERE slug='default'")
echo "org_id=$ORG"

echo "### 7. create project"
echo "proj_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" \
  --data-urlencode "name=Demo" --data-urlencode "description=" \
  "http://127.0.0.1:$HTTP_PORT/orgs/$ORG/projects") (expect 303)"
PROJ=$(docker exec "$PG_NAME" psql -U krill -d krill -t -A -c "SELECT id FROM projects WHERE organization_id=$ORG ORDER BY id DESC LIMIT 1")
echo "proj_id=$PROJ"

echo "### 8. create environment"
echo "env_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" \
  --data-urlencode "name=production" \
  "http://127.0.0.1:$HTTP_PORT/orgs/$ORG/projects/$PROJ/environments") (expect 303)"
ENV=$(docker exec "$PG_NAME" psql -U krill -d krill -t -A -c "SELECT id FROM environments WHERE project_id=$PROJ ORDER BY id DESC LIMIT 1")
echo "env_id=$ENV"

echo "### 9. create app"
APPBASE="http://127.0.0.1:$HTTP_PORT/orgs/$ORG/projects/$PROJ/environments/$ENV"
echo "app_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" \
  --data-urlencode "name=$APP" --data-urlencode "image=nginx" --data-urlencode "tag=alpine" \
  --data-urlencode "domain=" --data-urlencode "port=80" --data-urlencode "env=" \
  "$APPBASE/apps") (expect 303)"
ID=$(docker exec "$PG_NAME" psql -U krill -d krill -t -A -c "SELECT id FROM applications WHERE name='$APP'")
DOM=$(docker exec "$PG_NAME" psql -U krill -d krill -t -A -c "SELECT domain FROM applications WHERE name='$APP'")
echo "app_id=$ID domain=$DOM"

echo "### 10. deploy"
echo "deploy_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X POST \
  --data-urlencode "image=nginx" --data-urlencode "tag=alpine" \
  "$APPBASE/apps/$ID/deploy") (expect 303)"

echo "### 11. wait running (service krill-<appID>)"
run=0
for i in $(seq 1 60); do [ "$(docker service ls --filter name=krill-$ID --format '{{.Replicas}}')" = "1/1" ] && { run=1; break; }; sleep 2; done
docker service ls --filter name=krill-$ID --format 'SERVICE: {{.Name}} {{.Image}} {{.Replicas}}'
echo "service_running=$run (service krill-$ID)"

echo "### 12. traefik route"
out=000
for i in $(seq 1 8); do out=$(curl -s -m5 -H "Host: $DOM" -o /tmp/e2e_resp.txt -w '%{http_code}' http://127.0.0.1:80/); [ "$out" = "200" ] && break; sleep 2; done
echo "traefik_http=$out (expect 200)"
echo "traefik_body_nginx=$(grep -ci nginx /tmp/e2e_resp.txt 2>/dev/null || echo 0)"

echo "### 13. save env + redeploy"
echo "env_save_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X POST \
  --data-urlencode $'env=FOO=bar\nBAZ=qux' "$APPBASE/apps/$ID/env") (expect 303)"
echo "redeploy_http=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X POST \
  --data-urlencode "image=nginx" --data-urlencode "tag=1.27-alpine" "$APPBASE/apps/$ID/deploy") (expect 303)"
for i in $(seq 1 30); do docker service ls --filter name=krill-$ID --format '{{.Image}}' | grep -q '1.27' && break; sleep 2; done
echo "service_image_now=$(docker service ls --filter name=krill-$ID --format '{{.Image}}')"
echo "env_applied=$(docker service inspect krill-$ID --format '{{json .Spec.TaskTemplate.ContainerSpec.Env}}' 2>/dev/null)"
out2=$(curl -s -m 5 -H "Host: $DOM" -o /dev/null -w '%{http_code}' "http://127.0.0.1:80/")
echo "traefik_after_update_http=$out2 (expect 200, no downtime)"

echo "### 14. krill log tail"
tail -12 "$LOG"
echo "### DONE"
