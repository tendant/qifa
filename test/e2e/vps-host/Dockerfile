# A fresh VPS, as a cloud provider hands one over: Ubuntu 24.04 with
# systemd, sshd and curl, root logging in with your key, and nothing else:
# no Docker, no deploy user. Run privileged with the host cgroup namespace
# and volumes for /var/lib/docker and /var/lib/containerd, so the Docker the guide installs can run
# (see test/e2e/vps-e2e.sh).
FROM ubuntu:24.04

RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus openssh-server curl ca-certificates iptables \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /run/sshd

COPY test-keys.service /etc/systemd/system/test-keys.service
RUN systemctl enable test-keys.service ssh.service \
 && systemctl mask getty@tty1.service console-getty.service systemd-logind.service

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
