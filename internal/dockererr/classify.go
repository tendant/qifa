// Package dockererr turns raw Docker and SSH failure output into a diagnosis
// an operator can act on.
//
// Most qifa failures on a remote host surface as "Process exited with status
// 1" plus whatever Docker printed to stderr. That output usually does say what
// went wrong ("dial tcp: lookup ghcr.io: no such host"), but it is buried in a
// wall of pull progress and it never says what to *do*. Classify maps the
// well-known failure strings to a short summary, an actionable hint, and a
// retryable flag so callers can both retry transient network faults and print
// something useful when the retries run out.
package dockererr

import (
	"errors"
	"strings"
)

type Category string

const (
	CategoryDNS            Category = "dns"
	CategoryTimeout        Category = "network-timeout"
	CategoryRefused        Category = "connection-refused"
	CategoryUnreachable    Category = "network-unreachable"
	CategoryReset          Category = "connection-reset"
	CategoryTLSHandshake   Category = "tls-handshake"
	CategoryTLSTrust       Category = "tls-trust"
	CategoryHTTPSMismatch  Category = "registry-scheme"
	CategoryDaemonProxy    Category = "daemon-proxy"
	CategoryAuth           Category = "registry-auth"
	CategoryForbidden      Category = "registry-forbidden"
	CategoryNotFound       Category = "image-not-found"
	CategoryRateLimit      Category = "registry-rate-limit"
	CategoryDisk           Category = "disk-full"
	CategoryPlatform       Category = "platform-mismatch"
	CategoryDaemonDown     Category = "docker-daemon-down"
	CategoryDaemonPerms    Category = "docker-socket-permissions"
	CategoryDockerMissing  Category = "docker-not-installed"
	CategoryDigestMismatch Category = "digest-mismatch"
	CategoryUnknown        Category = "unknown"
)

// Cause is what Classify concluded about a failure.
type Cause struct {
	Category Category
	// Summary is a single line naming the fault, suitable for the first line
	// of an error message.
	Summary string
	// Hint is a short indented list of things to check or try. May be empty.
	Hint string
	// Retryable marks faults that are commonly transient — a retry with
	// backoff has a real chance of succeeding.
	Retryable bool
	// Network marks faults where probing the host's connectivity to the
	// registry adds information.
	Network bool
}

type rule struct {
	patterns []string
	cause    Cause
}

// Bare HTTP status numbers are deliberately not used as patterns: a sha256
// digest in the output can contain "401" and would misclassify the failure.
//
// Ordered most-specific first: the first matching rule wins, so narrower
// signatures (proxyconnect, http-response-to-https-client) must precede the
// generic ones they contain (connection refused, timeout).
var rules = []rule{
	{
		patterns: []string{"proxyconnect tcp", "proxy connect"},
		cause: Cause{
			Category:  CategoryDaemonProxy,
			Summary:   "the Docker daemon's HTTP proxy is unreachable or misconfigured",
			Retryable: true,
			Network:   true,
			Hint: `- the daemon (not your shell) uses HTTP_PROXY/HTTPS_PROXY from its systemd unit
- inspect it: systemctl show docker --property=Environment
             cat /etc/systemd/system/docker.service.d/*.conf
- after editing: systemctl daemon-reload && systemctl restart docker
- a proxy set for the shell only will NOT be used by docker pull`,
		},
	},
	{
		patterns: []string{"server gave http response to https client"},
		cause: Cause{
			Category: CategoryHTTPSMismatch,
			Summary:  "the registry speaks plain HTTP but Docker tried HTTPS",
			Hint: `- add the registry to the host's insecure registries and restart docker:
    /etc/docker/daemon.json -> {"insecure-registries": ["registry.example.com:5000"]}
    systemctl restart docker
- or put TLS in front of the registry (recommended for anything non-local)`,
		},
	},
	{
		patterns: []string{"no such host", "temporary failure in name resolution", "name or service not known", "server misbehaving"},
		cause: Cause{
			Category:  CategoryDNS,
			Summary:   "the host cannot resolve the registry hostname (DNS failure)",
			Retryable: true,
			Network:   true,
			Hint: `- check the host's resolvers: cat /etc/resolv.conf ; getent hosts <registry>
- private registries often need an internal DNS server or an /etc/hosts entry
- Docker Hub needs registry-1.docker.io, auth.docker.io and
  production.cloudflare.docker.com to resolve, not just docker.io`,
		},
	},
	{
		patterns: []string{"tls handshake timeout"},
		cause: Cause{
			Category:  CategoryTLSHandshake,
			Summary:   "TLS handshake with the registry timed out",
			Retryable: true,
			Network:   true,
			Hint: `- usually a slow/blocked path to the registry, or an MTU problem on a
  tunnel/overlay link (try: ping -M do -s 1400 <registry>)
- a transparent proxy or firewall that drops long TLS records causes this
- retrying often succeeds; persistent failures are a network path problem`,
		},
	},
	{
		patterns: []string{"certificate signed by unknown authority", "x509: certificate", "certificate has expired", "certificate is valid for"},
		cause: Cause{
			Category: CategoryTLSTrust,
			Summary:  "the host does not trust the registry's TLS certificate",
			Network:  true,
			Hint: `- install the registry CA on the host:
    cp ca.crt /usr/local/share/ca-certificates/ && update-ca-certificates
    and for docker specifically: /etc/docker/certs.d/<registry>/ca.crt
    then: systemctl restart docker
- "certificate has expired or is not yet valid" is often a wrong host clock —
  check the clock line in the diagnostics below`,
		},
	},
	{
		patterns: []string{"connection refused"},
		cause: Cause{
			Category:  CategoryRefused,
			Summary:   "the registry refused the connection",
			Retryable: true,
			Network:   true,
			Hint: `- the registry is down, or listening on a different port than the image
  reference implies (registry.example.com:5000/app vs :443)
- check from the host: curl -v https://<registry>/v2/`,
		},
	},
	{
		patterns: []string{"no route to host", "network is unreachable"},
		cause: Cause{
			Category:  CategoryUnreachable,
			Summary:   "the registry is not routable from this host",
			Retryable: true,
			Network:   true,
			Hint: `- egress firewall / security group is likely blocking 443 outbound
- verify from the host itself: curl -v https://<registry>/v2/`,
		},
	},
	{
		patterns: []string{"connection reset by peer", "unexpected eof", "read: connection reset", "http2: client connection lost", "unexpected end of json input"},
		cause: Cause{
			Category:  CategoryReset,
			Summary:   "the connection to the registry was reset mid-transfer",
			Retryable: true,
			Network:   true,
			Hint: `- flaky link, an idle-timeout on a NAT/firewall, or an MTU mismatch on a
  VPN/overlay interface (large layers fail, small ones succeed)
- retrying usually gets further; if every attempt dies at a similar point,
  suspect MTU: ip link set dev <iface> mtu 1400`,
		},
	},
	{
		patterns: []string{"i/o timeout", "context deadline exceeded", "timeout exceeded while awaiting headers", "client.timeout exceeded", "timeout awaiting response headers"},
		cause: Cause{
			Category:  CategoryTimeout,
			Summary:   "the connection to the registry timed out",
			Retryable: true,
			Network:   true,
			Hint: `- packets to the registry are being dropped (firewall) or the link is very slow
- check egress rules for 443/tcp and try: curl -v --max-time 15 https://<registry>/v2/
- large images over a slow link may simply need a longer timeout`,
		},
	},
	{
		patterns: []string{"toomanyrequests", "rate limit", "429 too many requests", "status: 429"},
		cause: Cause{
			Category:  CategoryRateLimit,
			Summary:   "the registry rate-limited this host",
			Retryable: true,
			Hint: `- Docker Hub limits anonymous pulls per source IP; authenticate to raise it:
    registry: {server: docker.io, username: ..., password_env: ...}
- or mirror the image into a private registry`,
		},
	},
	{
		patterns: []string{"unauthorized", "authentication required", "401 unauthorized", "status: 401"},
		cause: Cause{
			Category: CategoryAuth,
			Summary:  "the registry rejected the credentials (unauthorized)",
			Hint: `- check registry.username and that registry.password_env is exported where
  qifa runs (qifa uploads a docker config to the host; it does not read the
  host's own ~/.docker/config.json)
- for ghcr.io the token needs read:packages
- token may simply have expired`,
		},
	},
	{
		patterns: []string{"denied: requested access", "403 forbidden", "status: 403", "forbidden", "insufficient_scope"},
		cause: Cause{
			Category: CategoryForbidden,
			Summary:  "the credentials are valid but not allowed to pull this repository",
			Hint: `- the account lacks read access to this specific repository
- private images always need registry credentials configured in qifa.yaml`,
		},
	},
	{
		patterns: []string{"no matching manifest for", "does not match the detected host platform", "exec format error"},
		cause: Cause{
			Category: CategoryPlatform,
			Summary:  "the image has no build for this host's CPU architecture",
			Hint: `- an arm64 laptop building for an amd64 server is the usual cause
- set builder.platform (e.g. linux/amd64), or list both for a manifest list:
    builder: {platform: "linux/amd64,linux/arm64"}`,
		},
	},
	{
		patterns: []string{"manifest unknown", "manifest for", "not found: manifest", "repository does not exist", "pull access denied"},
		cause: Cause{
			Category: CategoryNotFound,
			Summary:  "the registry has no such image or tag",
			Hint: `- check the image name and tag; a private repo with no credentials reports
  the same error as a missing one
- list what exists: curl -u user:pass https://<registry>/v2/<repo>/tags/list`,
		},
	},
	{
		patterns: []string{"no space left on device", "no space left"},
		cause: Cause{
			Category: CategoryDisk,
			Summary:  "the host ran out of disk space while unpacking the image",
			Hint: `- free space under /var/lib/docker: docker system prune -af --volumes
- check the filesystem holding it: df -h /var/lib/docker`,
		},
	},
	{
		patterns: []string{"digest mismatch", "manifest verification failed", "filesystem layer verification failed"},
		cause: Cause{
			Category:  CategoryDigestMismatch,
			Summary:   "the downloaded layer failed verification (corrupt transfer)",
			Retryable: true,
			Network:   true,
			Hint: `- a proxy or cache is mangling the response body, or the transfer is corrupt
- retry; if it persists, bypass any transparent HTTP proxy for the registry`,
		},
	},
	{
		patterns: []string{"permission denied while trying to connect to the docker daemon", "dial unix /var/run/docker.sock: connect: permission denied"},
		cause: Cause{
			Category: CategoryDaemonPerms,
			Summary:  "the SSH user may not talk to the Docker daemon",
			Hint: `- add the user to the docker group on the host, then reconnect:
    usermod -aG docker <user>   (a new SSH session is required to pick it up)`,
		},
	},
	{
		patterns: []string{"cannot connect to the docker daemon", "is the docker daemon running"},
		cause: Cause{
			Category: CategoryDaemonDown,
			Summary:  "the Docker daemon is not running on the host",
			Hint: `- start it: systemctl start docker (and: systemctl enable docker)
- inspect why it died: systemctl status docker ; journalctl -u docker -n 50`,
		},
	},
	{
		patterns: []string{"docker: command not found", "docker: not found", "command not found"},
		cause: Cause{
			Category: CategoryDockerMissing,
			Summary:  "docker is not installed on the host (or not on the SSH PATH)",
			Hint: `- install it: curl -fsSL https://get.docker.com | sh
- if it IS installed, a non-login SSH shell may not have /usr/bin on PATH`,
		},
	},
}

// Classify inspects raw command output (stderr and/or stdout) and returns what
// it recognises. ok is false when nothing matched.
func Classify(output string) (Cause, bool) {
	lower := strings.ToLower(output)
	for _, r := range rules {
		for _, p := range r.patterns {
			if strings.Contains(lower, p) {
				return r.cause, true
			}
		}
	}
	return Cause{Category: CategoryUnknown}, false
}

// ClassifyErr walks an error chain for remote command output and classifies it.
func ClassifyErr(err error) (Cause, bool) {
	if err == nil {
		return Cause{Category: CategoryUnknown}, false
	}
	if out := Output(err); out != "" {
		if cause, ok := Classify(out); ok {
			return cause, true
		}
	}
	return Classify(err.Error())
}

// Retryable reports whether err looks like a transient network fault worth
// another attempt.
func Retryable(err error) bool {
	cause, ok := ClassifyErr(err)
	return ok && cause.Retryable
}

// OutputCarrier is implemented by errors that captured the remote command's
// output — ssh.RemoteError does. Declared here (rather than importing ssh) so
// this package stays dependency-free and usable from ssh's own callers.
type OutputCarrier interface {
	RemoteOutput() string
}

// Output returns the captured remote output from an error chain, if any.
func Output(err error) string {
	var carrier OutputCarrier
	if errors.As(err, &carrier) {
		return carrier.RemoteOutput()
	}
	return ""
}
