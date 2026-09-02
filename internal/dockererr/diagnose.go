package dockererr

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Runner is the slice of ssh.Client this package needs: run one command on one
// host and hand back its output.
type Runner interface {
	Run(ctx context.Context, host, command string) (string, error)
}

// Check is one line of the host report.
type Check struct {
	Name   string
	Status string // "ok", "FAIL", "warn", "info"
	Detail string
}

// Report is what the host said about its own ability to reach the registry.
type Report struct {
	Host     string
	Registry string
	Checks   []Check
	// Err is set when the probe itself could not run (SSH down, etc.).
	Err error
}

func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == "FAIL" {
			return true
		}
	}
	return false
}

func (r Report) String() string {
	if r.Err != nil {
		return fmt.Sprintf("  host diagnostics (%s): could not run: %v\n", r.Host, r.Err)
	}
	if len(r.Checks) == 0 {
		return ""
	}
	width := 0
	for _, c := range r.Checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  host diagnostics (%s):\n", r.Host)
	for _, c := range r.Checks {
		detail := strings.TrimSpace(c.Detail)
		detail = strings.Join(strings.Fields(detail), " ") // one line, no runs of space
		if len(detail) > 300 {
			detail = detail[:300] + "…"
		}
		fmt.Fprintf(&b, "    %-*s  %-4s  %s\n", width, c.Name, c.Status, detail)
	}
	return b.String()
}

// RegistryHost returns the registry hostname an image reference points at, and
// the hostnames a pull actually has to reach (Docker Hub uses three).
//
//	ghcr.io/acme/app:v1        -> ghcr.io
//	registry.example.com:5000/x -> registry.example.com:5000
//	nginx:1.27                 -> registry-1.docker.io (plus auth + CDN hosts)
func RegistryHost(imageRef string) string {
	ref := imageRef
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	parts := strings.Split(ref, "/")
	if len(parts) > 1 {
		first := parts[0]
		if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
			if first == "docker.io" || first == "index.docker.io" {
				return "registry-1.docker.io"
			}
			return first
		}
	}
	return "registry-1.docker.io"
}

func isDockerHub(host string) bool {
	return host == "registry-1.docker.io" || host == "docker.io" || host == "index.docker.io"
}

// probeHost is an endpoint a pull has to reach. api marks the one that speaks
// the registry API: Docker Hub's token service and layer CDN are required for
// a pull but do not serve /v2/, so probing it there reports a 404 that means
// nothing.
type probeHost struct {
	hostport string
	api      bool
}

// probeHosts lists every endpoint that has to be reachable for a pull from
// registry to work.
func probeHosts(registry string) []probeHost {
	if isDockerHub(registry) {
		return []probeHost{
			{hostport: "registry-1.docker.io", api: true},
			{hostport: "auth.docker.io"},
			{hostport: "production.cloudflare.docker.com"},
		}
	}
	return []probeHost{{hostport: registry, api: true}}
}

func splitHostPort(hostport string) (string, string) {
	if i := strings.LastIndex(hostport, ":"); i > 0 && !strings.Contains(hostport[i+1:], "]") {
		return hostport[:i], hostport[i+1:]
	}
	return hostport, "443"
}

// Diagnose asks the remote host, in its own words, whether it can reach the
// registry: DNS, TCP, the /v2/ endpoint, the daemon's proxy settings, disk
// space and clock skew. It is best-effort — every check degrades to a "warn"
// line rather than failing the probe, because it runs when something has
// already gone wrong and partial information still helps.
func Diagnose(ctx context.Context, runner Runner, host, imageRef string) Report {
	registry := RegistryHost(imageRef)
	report := Report{Host: host, Registry: registry}
	if runner == nil {
		return report
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	out, err := runner.Run(ctx, host, diagnoseScript(registry, imageRef))
	if err != nil && strings.TrimSpace(out) == "" {
		// The probe runs under `sh -c … || true`, so a hard error here means
		// SSH itself is broken — worth reporting, but only if we got nothing.
		if raw := Output(err); raw != "" {
			out = raw
		} else {
			report.Err = err
			return report
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 3)
		if len(fields) < 3 {
			continue
		}
		check := Check{Name: fields[0], Status: fields[1], Detail: fields[2]}
		if check.Name == "clock" {
			check = clockCheck(check)
		}
		report.Checks = append(report.Checks, check)
	}
	return report
}

// clockCheck converts the host's epoch seconds into a skew relative to the
// machine running qifa. A host clock far in the past or future makes every
// registry TLS handshake fail with a confusing "certificate is not yet valid".
func clockCheck(c Check) Check {
	epoch, err := strconv.ParseInt(strings.TrimSpace(c.Detail), 10, 64)
	if err != nil {
		return Check{Name: "clock", Status: "warn", Detail: "could not read host clock"}
	}
	skew := time.Since(time.Unix(epoch, 0))
	if skew < 0 {
		skew = -skew
	}
	detail := fmt.Sprintf("%s (skew vs local: %s)", time.Unix(epoch, 0).UTC().Format(time.RFC3339), skew.Round(time.Second))
	if skew > 5*time.Minute {
		return Check{Name: "clock", Status: "FAIL", Detail: detail + " — large skew breaks registry TLS; run: timedatectl set-ntp true"}
	}
	return Check{Name: "clock", Status: "ok", Detail: detail}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// diagnoseScript builds one POSIX sh program that emits `name|status|detail`
// lines. Everything is guarded so a missing tool (curl, getent, systemctl)
// degrades to a warn line instead of aborting the script.
func diagnoseScript(registry, imageRef string) string {
	var b strings.Builder
	b.WriteString(`r() { printf '%s|%s|%s\n' "$1" "$2" "$3"; }
has() { command -v "$1" >/dev/null 2>&1; }

# resolve prints the addresses of $1 using whichever resolver the host has.
# Minimal images ship none of these, hence the has_resolver guard below: a
# missing tool must read as "not tested", never as "DNS is broken".
resolve() {
  out=""
  if has getent; then out=$(getent ahostsv4 "$1" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' '); fi
  if [ -z "$out" ] && has getent; then out=$(getent hosts "$1" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' '); fi
  if [ -z "$out" ] && has nslookup; then out=$(nslookup "$1" 2>/dev/null | awk '/^Address: /{print $2}' | tr '\n' ' '); fi
  if [ -z "$out" ] && has host; then out=$(host "$1" 2>/dev/null | awk '/has address/{print $4}' | tr '\n' ' '); fi
  if [ -z "$out" ] && has dig; then out=$(dig +short "$1" 2>/dev/null | tr '\n' ' '); fi
  printf '%s' "$out"
}
has_resolver() { has getent || has nslookup || has host || has dig; }

if has docker; then
  r docker-cli ok "$(docker --version 2>&1 | head -1)"
else
  r docker-cli FAIL "docker is not on PATH for this SSH user"
fi

d=$(docker info --format '{{.ServerVersion}}' 2>&1 | head -3 | tr '\n' ' ')
if docker info >/dev/null 2>&1; then
  r docker-daemon ok "server $d"
else
  r docker-daemon FAIL "$d"
fi
`)

	for _, endpoint := range probeHosts(registry) {
		name, port := splitHostPort(endpoint.hostport)
		fmt.Fprintf(&b, `
rh=%[1]s; rp=%[2]s
addrs=$(resolve "$rh")
if [ -n "$addrs" ]; then
  r "dns $rh" ok "$addrs"
elif has_resolver; then
  r "dns $rh" FAIL "no address (check /etc/resolv.conf and the host's DNS server)"
else
  r "dns $rh" warn "no resolver tool (getent/nslookup/host/dig) available to test"
fi

if has nc; then
  if nc -z -w 5 "$rh" "$rp" >/dev/null 2>&1; then
    r "tcp $rh:$rp" ok "connect succeeded"
  else
    r "tcp $rh:$rp" FAIL "cannot open a TCP connection (firewall or egress rule?)"
  fi
else
  r "tcp $rh:$rp" warn "nc not installed; cannot test the TCP path"
fi
`, shellQuote(name), shellQuote(port))
		if !endpoint.api {
			continue
		}
		b.WriteString(`
if has curl; then
  code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 "https://$rh:$rp/v2/" 2>/tmp/.qifa-curl-err)
  cerr=$(head -2 /tmp/.qifa-curl-err 2>/dev/null | tr '\n' ' '); rm -f /tmp/.qifa-curl-err
  case "$code" in
    200|401|403) r "https $rh/v2/" ok "HTTP $code (registry API reachable)" ;;
    000|"") r "https $rh/v2/" FAIL "no HTTP response: $cerr" ;;
    *) r "https $rh/v2/" warn "HTTP $code $cerr" ;;
  esac
else
  r "https $rh/v2/" warn "curl not installed; cannot test the registry API"
fi
`)
	}

	b.WriteString(`
prox=$( (systemctl show docker --property=Environment 2>/dev/null; cat /etc/systemd/system/docker.service.d/*.conf 2>/dev/null) | grep -i -o '[A-Za-z_]*_proxy=[^" ]*' | sort -u | tr '\n' ' ')
if [ -n "$prox" ]; then
  r daemon-proxy info "$prox (the daemon uses THIS, not your shell's proxy env)"
else
  r daemon-proxy info "none configured for the docker daemon"
fi

mirrors=$(docker info --format '{{join .RegistryConfig.Mirrors ","}}' 2>/dev/null)
insec=$(docker info --format '{{range .RegistryConfig.IndexConfigs}}{{if not .Secure}}{{.Name}} {{end}}{{end}}' 2>/dev/null)
r registry-config info "mirrors=[${mirrors}] insecure=[${insec}]"

root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null); [ -n "$root" ] || root=/var/lib/docker
avail=$(df -Pk "$root" 2>/dev/null | awk 'NR==2{print $4}')
availh=$(df -Ph "$root" 2>/dev/null | awk 'NR==2{print $4" free of "$2}')
if [ -z "$avail" ] && [ ! -d "$root" ]; then
  r disk info "$root is not on this machine (Docker Desktop or a remote daemon holds the images)"
elif [ -z "$avail" ]; then
  r disk warn "could not measure free space under $root"
elif [ "$avail" -lt 2097152 ]; then
  r disk FAIL "$root: $availh (under 2G — pulls will fail to unpack)"
else
  r disk ok "$root: $availh"
fi

r clock info "$(date -u +%s)"
`)

	if imageRef != "" {
		fmt.Fprintf(&b, `
if docker image inspect %[1]s >/dev/null 2>&1; then
  r image-cached info "%[2]s is already present on this host"
else
  r image-cached info "%[2]s is not cached locally; it must be pulled"
fi
`, shellQuote(imageRef), imageRef)
	}

	// Never let a failing probe mask the original error.
	return "{ " + b.String() + " } 2>/dev/null; exit 0"
}
