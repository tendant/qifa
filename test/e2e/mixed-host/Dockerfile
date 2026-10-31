# A server running both kinds of qifa app: systemd for native services and
# Docker for containers, sharing one kamal-proxy (the Docker one). Run
# privileged with the host cgroup namespace and a volume for /var/lib/docker
# (see test/e2e/mixed-e2e.sh).
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus openssh-server sudo curl ca-certificates docker.io iptables \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --shell /bin/bash deploy \
 && mkdir -p /run/sshd

COPY test-keys.service /etc/systemd/system/test-keys.service
RUN systemctl enable test-keys.service ssh.service docker.service \
 && systemctl mask getty@tty1.service console-getty.service systemd-logind.service

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
