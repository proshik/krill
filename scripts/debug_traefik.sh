#!/usr/bin/env bash
# Debug Traefik routing: bring up the stack, inspect the service labels and Traefik logs.
set -uo pipefail
R="$(cd "$(dirname "$0")/.." && pwd)"
cd "$R"
HTTP_PORT=18080; PG_PORT=55432; PG_NAME=krill-e2e-pg; APP=e2eweb

export KRILL_LISTEN_ADDR=:$HTTP_PORT
export KRILL_DATABASE_URL="postgres://krill:krill@localhost:$PG_PORT/krill?sslmode=disable"
export KRILL_ADMIN_EMAIL="admin@krill.local"; export KRILL_ADMIN_PASSWORD="changeme"
export KRILL_DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
export KRILL_BASE_DOMAIN="127-0-0-1.sslip.io"; export KRILL_NETWORK="krill-net"; export KRILL_COOKIE_SECURE=false
JAR=/tmp/dbg_cookies.txt; rm -f "$JAR"

cleanup(){ kill "${KPID:-0}" >/dev/null 2>&1||true; docker rm -f "$PG_NAME">/dev/null 2>&1||true; docker service rm krill-$APP krill-traefik>/dev/null 2>&1||true; }
trap cleanup EXIT

docker rm -f "$PG_NAME">/dev/null 2>&1||true
docker run -d --name "$PG_NAME" -e POSTGRES_USER=krill -e POSTGRES_PASSWORD=krill -e POSTGRES_DB=krill -p $PG_PORT:5432 postgres:16-alpine >/dev/null
for i in $(seq 1 30); do docker exec "$PG_NAME" pg_isready -U krill>/dev/null 2>&1 && break; sleep 1; done
docker service rm krill-$APP krill-traefik >/dev/null 2>&1||true

./bin/krill > /tmp/dbg_app.log 2>&1 & KPID=$!
for i in $(seq 1 50); do curl -fsS -o /dev/null "http://127.0.0.1:$HTTP_PORT/login" && break; sleep 0.5; done
for i in $(seq 1 30); do docker service ls --filter name=krill-traefik --format '{{.Replicas}}'|grep -q 1/1 && break; sleep 2; done

curl -s -o /dev/null -c "$JAR" --data-urlencode "email=$KRILL_ADMIN_EMAIL" --data-urlencode "password=$KRILL_ADMIN_PASSWORD" "http://127.0.0.1:$HTTP_PORT/login"
curl -s -o /dev/null -b "$JAR" --data-urlencode "name=$APP" --data-urlencode "image=nginx" --data-urlencode "tag=alpine" --data-urlencode "domain=" --data-urlencode "port=80" --data-urlencode "env=" "http://127.0.0.1:$HTTP_PORT/apps"
ID=$(docker exec "$PG_NAME" psql -U krill -d krill -t -A -c "SELECT id FROM applications WHERE name='$APP'")
curl -s -o /dev/null -b "$JAR" -X POST --data-urlencode "image=nginx" --data-urlencode "tag=alpine" "http://127.0.0.1:$HTTP_PORT/apps/$ID/deploy"
for i in $(seq 1 60); do [ "$(docker service ls --filter name=krill-$APP --format '{{.Replicas}}')" = "1/1" ] && break; sleep 2; done

echo "===== APP SERVICE LABELS (service-level) ====="
docker service inspect krill-$APP --format '{{json .Spec.Labels}}' | tr ',' '\n'
echo "===== APP SERVICE NETWORKS ====="
docker service inspect krill-$APP --format '{{json .Spec.TaskTemplate.Networks}}'
echo "===== TRAEFIK SERVICE ARGS ====="
docker service inspect krill-traefik --format '{{json .Spec.TaskTemplate.ContainerSpec.Args}}'
echo "===== TRAEFIK NETWORKS ====="
docker service inspect krill-traefik --format '{{json .Spec.TaskTemplate.Networks}}'
echo "===== TRAEFIK LOGS (grep provider/swarm/error/router) ====="
docker service logs krill-traefik 2>&1 | grep -iE 'swarm|provider|error|router|level=warn|level=erro' | tail -40
echo "===== curl through traefik ====="
curl -s -m5 -H "Host: $APP.127-0-0-1.sslip.io" -o /dev/null -w 'host80=%{http_code}\n' http://127.0.0.1:80/
echo "===== DONE ====="
