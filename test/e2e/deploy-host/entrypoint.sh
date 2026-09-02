#!/bin/sh
set -eu

mkdir -p /run/sshd /test-ssh /root/.ssh
chmod 700 /test-ssh /root/.ssh || true

# Copy the mounted public key to a root-owned authorized_keys rather than
# letting sshd read it straight off the bind mount. A bind mount keeps the
# HOST's uid: on a Linux CI runner the file arrives owned by uid 1001, and
# sshd's StrictModes then refuses every key with "bad ownership or modes",
# so the whole harness fails at the first SSH connection. It happens to work
# on Docker Desktop only because virtiofs reports the files as root-owned.
if [ -f /test-ssh/authorized_keys ]; then
  cat /test-ssh/authorized_keys > /root/.ssh/authorized_keys
  chown root:root /root/.ssh/authorized_keys
  chmod 600 /root/.ssh/authorized_keys
fi

ssh-keygen -A >/dev/null 2>&1
/usr/sbin/sshd -D -e &

export DOCKER_TLS_CERTDIR=""
exec dockerd-entrypoint.sh \
  --tls=false \
  --host=tcp://0.0.0.0:2375 \
  --host=unix:///var/run/docker.sock \
  --insecure-registry=zottest:5001
