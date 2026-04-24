FROM docker:27-dind

RUN apk upgrade --no-cache && apk add --no-cache bash curl openssh-server openssh-sftp-server

COPY sshd_config /etc/ssh/sshd_config
COPY kamal-proxy /usr/local/bin/kamal-proxy
COPY entrypoint.sh /usr/local/bin/test-entrypoint

RUN chmod +x /usr/local/bin/kamal-proxy /usr/local/bin/test-entrypoint

EXPOSE 22 2375 3000

ENTRYPOINT ["/usr/local/bin/test-entrypoint"]
