FROM golang:1.25.1-alpine AS kamal-proxy-build

RUN GOBIN=/out go install github.com/basecamp/kamal-proxy/cmd/kamal-proxy@v0.9.0

FROM docker:27-dind

RUN apk upgrade --no-cache && apk add --no-cache bash curl openssh-server openssh-sftp-server

COPY sshd_config /etc/ssh/sshd_config
COPY --from=kamal-proxy-build /out/kamal-proxy /usr/local/bin/kamal-proxy
COPY entrypoint.sh /usr/local/bin/test-entrypoint

RUN chmod +x /usr/local/bin/kamal-proxy /usr/local/bin/test-entrypoint

EXPOSE 22 80 2375

ENTRYPOINT ["/usr/local/bin/test-entrypoint"]
