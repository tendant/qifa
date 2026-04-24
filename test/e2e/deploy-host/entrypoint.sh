#!/bin/sh
set -eu

mkdir -p /run/sshd /test-ssh
chmod 700 /test-ssh || true

ssh-keygen -A >/dev/null 2>&1
/usr/sbin/sshd -D -e &

export DOCKER_TLS_CERTDIR=""
exec dockerd-entrypoint.sh \
  --tls=false \
  --host=tcp://0.0.0.0:2375 \
  --host=unix:///var/run/docker.sock \
  --insecure-registry=zot:5000
