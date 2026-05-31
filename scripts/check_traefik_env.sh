#!/usr/bin/env bash
# Минимальная проверка: реально ли DOCKER_API_VERSION попадает в ContainerSpec.Env Traefik,
# и помогает ли это. Полностью свежий traefik (ждём удаления старого).
set -uo pipefail
R=.
cd "$R"
HTTP_PORT=18080; PG_PORT=55432; PG_NAME=krill-e2e-pg; APP=e2eweb

export KRILL_LISTEN_ADDR=:$HTTP_PORT
export KRILL_DATABASE_URL="postgres://krill:krill@localhost:$PG_PORT/krill?sslmode=disable"
export KRILL_ADMIN_EMAIL="admin@krill.local"; export KRILL_ADMIN_PASSWORD="changeme"
export KRILL_DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
export KRILL_BASE_DOMAIN="127-0-0-1.sslip.io"; export KRILL_NETWORK="krill-net"; export KRILL_COOKIE_SECURE=false
JAR=/tmp/chk_cookies.txt; rm -f "$JAR"

cleanup(){ kill "${KPID:-0}" >/dev/null 2>&1||true; docker rm -f "$PG_NAME">/dev/null 2>&1||true; docker service rm krill-$APP krill-traefik>/dev/null 2>&1||true; }
trap cleanup EXIT

echo "### ensure traefik fully removed"
docker service rm krill-traefik >/dev/null 2>&1 || true
for i in $(seq 1 30); do docker service ls --format '{{.Name}}' | grep -q '^krill-traefik$' || break; sleep 1; done
echo "traefik_present_before=$(docker service ls --format '{{.Name}}' | grep -c '^krill-traefik$')"

docker rm -f "$PG_NAME">/dev/null 2>&1||true
docker run -d --name "$PG_NAME" -e POSTGRES_USER=krill -e POSTGRES_PASSWORD=krill -e POSTGRES_DB=krill -p $PG_PORT:5432 postgres:16-alpine >/dev/null
for i in $(seq 1 30); do docker exec "$PG_NAME" pg_isready -U krill>/dev/null 2>&1 && break; sleep 1; done

./bin/krill > /tmp/chk_app.log 2>&1 & KPID=$!
for i in $(seq 1 50); do curl -fsS -o /dev/null "http://127.0.0.1:$HTTP_PORT/login" && break; sleep 0.5; done
for i in $(seq 1 30); do docker service ls --filter name=krill-traefik --format '{{.Replicas}}'|grep -q 1/1 && break; sleep 2; done

echo "### TRAEFIK ContainerSpec.Env (the real check)"
docker service inspect krill-traefik --format '{{json .Spec.TaskTemplate.ContainerSpec.Env}}'

echo "### deploy an app"
curl -s -o /dev/null -c "$JAR" --data-urlencode "email=$KRILL_ADMIN_EMAIL" --data-urlencode "password=$KRILL_ADMIN_PASSWORD" "http://127.0.0.1:$HTTP_PORT/login"
curl -s -o /dev/null -b "$JAR" --data-urlencode "name=$APP" --data-urlencode "image=nginx" --data-urlencode "tag=alpine" --data-urlencode "domain=" --data-urlencode "port=80" --data-urlencode "env=" "http://127.0.0.1:$HTTP_PORT/apps"
ID=$(docker exec "$PG_NAME" psql -U krill -d krill -t -A -c "SELECT id FROM applications WHERE name='$APP'")
curl -s -o /dev/null -b "$JAR" -X POST --data-urlencode "image=nginx" --data-urlencode "tag=alpine" "http://127.0.0.1:$HTTP_PORT/apps/$ID/deploy"
for i in $(seq 1 60); do [ "$(docker service ls --filter name=krill-$APP --format '{{.Replicas}}')" = "1/1" ] && break; sleep 2; done

echo "### wait for traefik to pick up route (10s)"; sleep 10
echo "### recent traefik logs (api errors?)"
echo "api_too_old_count=$(docker service logs krill-traefik 2>&1 | grep -c '1.24 is too old')"
echo "### curl through traefik"
curl -s -m5 -H "Host: $APP.127-0-0-1.sslip.io" -o /tmp/chk_resp.txt -w 'host80=%{http_code}\n' http://127.0.0.1:80/
echo "body_nginx=$(grep -ci 'nginx\|Welcome' /tmp/chk_resp.txt 2>/dev/null || echo 0)"
echo "### DONE"
