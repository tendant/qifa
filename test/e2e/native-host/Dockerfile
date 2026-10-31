# A plain Linux server for native-deploy e2e: systemd as PID 1, sshd, sudo,
# curl. No Docker. Run privileged with the host cgroup namespace (see
# test/e2e/native-e2e.sh).
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus openssh-server sudo curl ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --shell /bin/bash deploy \
 && mkdir -p /run/sshd

COPY test-keys.service /etc/systemd/system/test-keys.service
RUN systemctl enable test-keys.service ssh.service \
 && systemctl mask getty@tty1.service console-getty.service systemd-logind.service

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
