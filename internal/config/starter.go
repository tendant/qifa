package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
)

// Starter is what `qifa init` knows when it writes a config: the smallest
// one that deploys as written, shaped by the project directory and the
// flags. The zero value is the plain starter: nginx on this machine.
type Starter struct {
	Service string // default "myapp"
	Host    string // an SSH host; "" deploys to this machine ("local")
	Domain  string // serve through kamal-proxy at https://Domain
	Build   bool   // build the Dockerfile in Context on each host instead of pulling nginx
	Context string // the build context, relative to where qifa runs; default "."
	AppPort int    // what the container listens on; default 80
}

// DetectStarter fills a Starter from dir, where the config will live: the
// service is named after the directory, and a Dockerfile there means
// building it, on the port its last EXPOSE names.
func DetectStarter(dir string) Starter {
	var s Starter
	if abs, err := filepath.Abs(dir); err == nil {
		s.Service = serviceNameFrom(filepath.Base(abs))
	}
	if data, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err == nil {
		s.Build = true
		s.Context = filepath.Clean(dir)
		s.AppPort = exposedPort(data)
	}
	return s
}

var nonServiceChars = regexp.MustCompile(`[^a-z0-9]+`)

// serviceNameFrom turns a directory name into a service name: lower case,
// letters and digits joined by single dashes. qifa names containers after
// the service, so it keeps to what Docker accepts everywhere.
func serviceNameFrom(name string) string {
	name = strings.Trim(nonServiceChars.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return ""
	}
	return name
}

// exposedPort is the first port of a Dockerfile's last EXPOSE, the one the
// final stage of a multi-stage build declares; 0 when there is none, or it
// is a build argument.
func exposedPort(dockerfile []byte) int {
	port := 0
	scanner := bufio.NewScanner(bytes.NewReader(dockerfile))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || !strings.EqualFold(fields[0], "EXPOSE") {
			continue
		}
		p, err := strconv.Atoi(strings.SplitN(fields[1], "/", 2)[0])
		if err != nil || p < 1 || p > 65535 {
			port = 0
			continue
		}
		port = p
	}
	return port
}

// Check rejects flag values that would not survive being written into YAML
// unquoted; whether they make a working config is Load's job.
func (s Starter) Check() error {
	for name, v := range map[string]string{"--host": s.Host, "--domain": s.Domain} {
		if v != "" && strings.ContainsAny(v, " \t\n,[]{}#:'\"") && !(name == "--host" && hostWithPort(v)) {
			return fmt.Errorf("%s %q: give one bare name or address", name, v)
		}
	}
	if strings.Contains(s.Domain, "/") {
		return fmt.Errorf("--domain %q: give the name only, without https:// or a path", s.Domain)
	}
	return nil
}

// hostWithPort allows the one colon an SSH host may carry: host:port.
func hostWithPort(v string) bool {
	host, port, ok := strings.Cut(v, ":")
	if !ok || host == "" || strings.ContainsAny(host, " \t\n,[]{}#:'\"") {
		return false
	}
	_, err := strconv.Atoi(port)
	return err == nil
}

// YAML renders the starter config.
func (s Starter) YAML() string {
	if s.Service == "" {
		s.Service = "myapp"
	}
	if s.AppPort == 0 {
		s.AppPort = 80
	}
	if s.Context == "" {
		s.Context = "."
	}
	if strings.ContainsAny(s.Context, " \t:#\"'[]{},&*!|>%@`\\") {
		s.Context = strconv.Quote(s.Context) // a YAML double-quoted string
	}
	// Addr is the machine itself, for the URL and the DNS hint: the host
	// without an SSH user or port.
	addr := s.Host
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		addr = addr[i+1:]
	}
	if host, _, ok := strings.Cut(addr, ":"); ok {
		addr = host
	}
	data := struct {
		Starter
		Addr string
	}{s, addr}
	var buf bytes.Buffer
	if err := starterTemplate.Execute(&buf, data); err != nil {
		panic(err) // the template is fixed; tests render every shape
	}
	return buf.String()
}

// WriteStarter writes s to path, creating its directory.
func WriteStarter(path string, s Starter) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(s.YAML()), 0o644)
}

// WriteSample writes the plain starter.
func WriteSample(path string) error { return WriteStarter(path, Starter{}) }

// sampleConfig is the plain starter. Each commented block in it loads once
// uncommented; TestSampleConfigBlocksLoad keeps that true.
var sampleConfig = Starter{}.YAML()

var starterTemplate = template.Must(template.New("starter").Parse(
	`
{{- if .Build -}}
# qifa.yaml: builds the Dockerfile and deploys it as written, once your app
# answers 2xx at healthcheck.path (/ below; set it to your health route).
{{- if .Domain}}
# Point DNS for {{.Domain}} at {{if .Addr}}{{.Addr}}{{else}}this machine{{end}} and open ports 80 and 443 there first.
# Then run "qifa deploy" and open https://{{.Domain}}.
{{- else}}
# Then run "qifa deploy" and open http://{{if .Addr}}{{.Addr}}{{else}}localhost{{end}}:8765.
{{- end}}
# Edit from there; "qifa config" checks your changes.
{{- else if .Domain -}}
# qifa.yaml: deploys as written once DNS for {{.Domain}} points at
# {{if .Addr}}{{.Addr}}{{else}}this machine{{end}} and ports 80 and 443 are open there. Run "qifa deploy",
# then open https://{{.Domain}}. Edit from there; "qifa config" checks your changes.
{{- else -}}
# qifa.yaml: deploys as written. Run "qifa deploy", then open
# http://{{if .Addr}}{{.Addr}}{{else}}localhost{{end}}:8765. Edit from there; "qifa config" checks your changes.
{{- end}}
service: {{.Service}}
{{- if .Build}}
image: {{printf "%-20s" .Service}} # built from the Dockerfile; see builder: below
{{- else}}
image: nginx:1.27-alpine    # an image with a tag, or set builder: below
{{- end}}

servers:
  web:
{{- if .Host}}
    hosts: {{printf "%-16s" (printf "[%s]" .Host)}} # over SSH; see ssh: below
{{- else}}
    hosts: [local]          # "local" is this machine (no SSH); or SSH hosts: [10.0.0.11]
{{- end}}
{{- if not .Domain}}
    port: 8765              # published on the host
{{- end}}
    app_port: {{printf "%-13d" .AppPort}} # what the container listens on
{{- if .Domain}}
    proxy: true             # served through kamal-proxy; see proxy: below
{{- else}}
    proxy: false            # true (with proxy: below) for a domain and HTTPS
{{- end}}

healthcheck:
  path: /                   # must answer 2xx before the deploy counts
{{- if .Build}}
{{- if .Domain}}

# Uploads this directory to each host and builds there; no registry needed.
{{- else}}

# --- Build your own image instead of pulling one ---------------------------
# Uploads this directory to each host and builds there; no registry needed.
{{- end}}
builder:
  host: per_target
  context: {{.Context}}
{{- else}}

# --- Build your own image instead of pulling one ---------------------------
# Uploads this directory to each host and builds there; no registry needed.
#builder:
#  host: per_target
#  context: .
{{- end}}

# --- Environment ----------------------------------------------------------
#env:
#  clear:
#    APP_ENV: production
#  secret: [DATABASE_URL]          # read from your shell at deploy time
{{- if .Domain}}

# The first deploy starts kamal-proxy on the host (ports 80 and 443) if it
# is not running, and kamal-proxy gets a Let's Encrypt certificate.
proxy:
  host: {{.Domain}}
  app_port: {{.AppPort}}
  ssl: true
  tls:
    source: kamal
{{- else}}

# --- A domain with HTTPS, through kamal-proxy -----------------------------
# Set servers.web.proxy to true and drop servers.web.port. The first deploy
# starts kamal-proxy on the host (ports 80 and 443) if it is not running.
#proxy:
#  host: app.example.com
#  app_port: {{.AppPort}}
#  ssl: true
#  tls:
#    source: kamal                 # kamal-proxy gets a Let's Encrypt certificate
{{- end}}

# --- Remote hosts over SSH ------------------------------------------------
{{- if .Host}}
# Without this block qifa connects as your SSH config (~/.ssh/config) says.
{{- end}}
#ssh:
#  user: deploy
#  key: ~/.ssh/id_ed25519
`))
