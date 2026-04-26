#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
E2E_DIR="$ROOT_DIR/test/e2e"
TMP_DIR="$E2E_DIR/.tmp"
SSH_DIR="$TMP_DIR/ssh"
BIN_DIR="$TMP_DIR/bin"
RUN_ROOT=${RUN_ROOT:-$(mktemp -d /tmp/qifa-zot-e2e-XXXXXX)}
WEB_DIR="$RUN_ROOT/web-run"
WORKER_DIR="$RUN_ROOT/worker-run"
COMPOSE_FILE="$E2E_DIR/docker-compose.yml"
REGISTRY_HOST="zottest:5001"
REGISTRY_USER="testuser"
REGISTRY_PASSWORD_VALUE="testpass"

cleanup() {
  if [[ "${KEEP_E2E_ENV:-0}" != "1" ]]; then
    docker compose -f "$COMPOSE_FILE" down -v >/dev/null 2>&1 || true
  fi
  if [[ "${KEEP_E2E_ENV:-0}" != "1" ]]; then
    rm -rf "$RUN_ROOT"
  fi
}

trap cleanup EXIT

mkdir -p "$SSH_DIR" "$BIN_DIR" "$WEB_DIR" "$WORKER_DIR"

if [[ ! -f "$SSH_DIR/id_ed25519" ]]; then
  ssh-keygen -q -t ed25519 -N "" -f "$SSH_DIR/id_ed25519"
fi
cp "$SSH_DIR/id_ed25519.pub" "$SSH_DIR/authorized_keys"

docker compose -f "$COMPOSE_FILE" down -v >/dev/null 2>&1 || true
docker compose -f "$COMPOSE_FILE" up -d --build zot deploy-host

wait_for() {
  local label=$1
  local command=$2
  local attempts=${3:-60}
  local delay=${4:-1}

  for _ in $(seq 1 "$attempts"); do
    if bash -lc "$command" >/dev/null 2>&1; then
      return 0
    fi
    sleep "$delay"
  done

  echo "timed out waiting for $label" >&2
  return 1
}

wait_for "zot registry" "curl -fsS -u $REGISTRY_USER:$REGISTRY_PASSWORD_VALUE http://127.0.0.1:5001/v2/"
wait_for "deploy host ssh" "ssh-keyscan -p 2222 127.0.0.1 >/dev/null 2>&1"
wait_for "deploy host docker daemon" "unset DOCKER_TLS_VERIFY DOCKER_CERT_PATH DOCKER_TLS_CERTDIR; DOCKER_HOST=tcp://127.0.0.1:23750 docker info"

HOME_DIR="$TMP_DIR/home"
mkdir -p "$HOME_DIR/.ssh"
cp "$SSH_DIR/id_ed25519" "$HOME_DIR/.ssh/id_ed25519"
chmod 600 "$HOME_DIR/.ssh/id_ed25519"
ssh-keyscan -p 2222 127.0.0.1 > "$HOME_DIR/.ssh/known_hosts"
printf 'zottest 127.0.0.1\n' > "$TMP_DIR/hosts.aliases"

export HOME="$HOME_DIR"
export DOCKER_HOST="tcp://127.0.0.1:23750"
export HOSTALIASES="$TMP_DIR/hosts.aliases"
export REGISTRY_PASSWORD="$REGISTRY_PASSWORD_VALUE"
unset DOCKER_TLS_CERTDIR
unset DOCKER_TLS_VERIFY
unset DOCKER_CERT_PATH

go build -o "$BIN_DIR/qifa" "$ROOT_DIR/cmd/qifa"

cat > "$WEB_DIR/qifa.yaml" <<EOF
service: demo-web
image: $REGISTRY_HOST/demo-web

servers:
  web:
    hosts:
      - 127.0.0.1:2222
    port: 3000

proxy:
  host: localhost
  app_port: 3000
  healthcheck:
    path: /up
    interval: 2s
    timeout: 5s

registry:
  server: $REGISTRY_HOST
  username: $REGISTRY_USER
  password_env: REGISTRY_PASSWORD

env:
  clear:
    APP_ENV: test
  secret: []

builder:
  context: $E2E_DIR/demo-app
  dockerfile: $E2E_DIR/demo-app/Dockerfile

ssh:
  user: root
  key: ~/.ssh/id_ed25519
EOF

cat > "$WORKER_DIR/qifa.yaml" <<EOF
service: demo-worker
image: $REGISTRY_HOST/demo-worker

servers:
  worker:
    hosts:
      - 127.0.0.1:2222

proxy:
  host: localhost
  app_port: 3000
  healthcheck:
    path: /up
    interval: 2s
    timeout: 5s

registry:
  server: $REGISTRY_HOST
  username: $REGISTRY_USER
  password_env: REGISTRY_PASSWORD

env:
  clear:
    APP_ENV: test
  secret: []

builder:
  context: $E2E_DIR/demo-app
  dockerfile: $E2E_DIR/demo-app/Dockerfile

ssh:
  user: root
  key: ~/.ssh/id_ed25519
EOF

(
  cd "$WEB_DIR"
  "$BIN_DIR/qifa" deploy
  curl -fsS -H 'Host: localhost' http://127.0.0.1:8080/up
  curl -fsS -H 'Host: localhost' http://127.0.0.1:8080/version
  "$BIN_DIR/qifa" deploy
  curl -fsS -H 'Host: localhost' http://127.0.0.1:8080/up
  "$BIN_DIR/qifa" status
  "$BIN_DIR/qifa" logs
  "$BIN_DIR/qifa" app exec "echo web-ok"
)

(
  cd "$WORKER_DIR"
  "$BIN_DIR/qifa" deploy
  sleep 1
  "$BIN_DIR/qifa" deploy
  "$BIN_DIR/qifa" rollback
  "$BIN_DIR/qifa" status
  "$BIN_DIR/qifa" logs
  "$BIN_DIR/qifa" app exec "echo worker-ok"
)

docker --host "$DOCKER_HOST" ps
docker --host "$DOCKER_HOST" images | grep "$REGISTRY_HOST/demo"

echo "zot e2e test completed"
if [[ "${KEEP_E2E_ENV:-0}" == "1" ]]; then
  echo "environment kept running"
fi
