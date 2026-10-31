package native

import (
	"fmt"
	"path"
	"strings"

	"github.com/tendant/qifa/internal/ssh"
)

// Remote scripts. Every value interpolated here is either validated by config
// to be shell-safe (service, role, user, dir, shared_dirs) or passed through
// ssh.Quote. They run as the deploy user, except setupScript, which runs as
// root.

// q is a short alias for ssh.Quote.
func q(s string) string { return ssh.Quote(s) }

func uploadPath(dir, id string) string { return dir + "/releases/." + id + ".tgz" }

// stageScript turns an uploaded archive into releases/<id>. It never touches
// `current`, so a failure here leaves the running release alone.
func stageScript(dir, id, sha string, sharedDirs, execs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "set -eu\ncd %s/releases\nid=%s\nsha=%s\ntgz=%s\n", q(dir), q(id), q(sha), q("."+id+".tgz"))
	b.WriteString(`for f in *.tmp; do [ -e "$f" ] && rm -rf -- "$f"; done
for f in .*.tgz; do [ -e "$f" ] && [ "$f" != "$tgz" ] && rm -f -- "$f"; done
if [ -d "$id" ]; then
  rm -f -- "$tgz"
  have=$(cat "$id/.qifa-checksum" 2>/dev/null || true)
  if [ "$have" = "$sha" ]; then echo reused; exit 0; fi
  if [ "$(readlink ../current 2>/dev/null || true)" = "releases/$id" ]; then
    echo "release $id is live but this build of it has different contents; deploy it under a new --version" >&2
    exit 3
  fi
  rm -rf -- "$id"
fi
got=$(sha256sum "$tgz" | cut -d' ' -f1)
if [ "$got" != "$sha" ]; then
  rm -f -- "$tgz"
  echo "upload checksum mismatch: got $got, expected $sha" >&2
  exit 4
fi
mkdir -- "$id.tmp"
tar -xzf "$tgz" -C "$id.tmp"
rm -f -- "$tgz"
`)
	for _, d := range sharedDirs {
		target := dir + "/shared/" + d
		link := `"$id.tmp"/` + q(d)
		fmt.Fprintf(&b, "rm -rf -- %s\nmkdir -p -- \"$(dirname -- %s)\"\nln -s -- %s %s\n", link, link, q(target), link)
	}
	for _, e := range execs {
		check := `"$id.tmp"/` + q(strings.TrimPrefix(e, "./"))
		if path.IsAbs(e) {
			check = q(e)
		}
		fmt.Fprintf(&b, "if [ ! -x %s ]; then echo %s >&2; rm -rf -- \"$id.tmp\"; exit 5; fi\n",
			check, q(fmt.Sprintf("%s is missing from the release or not executable (check native.artifact and native.exec)", e)))
	}
	b.WriteString(`printf '%s\n' "$sha" > "$id.tmp/.qifa-checksum"
chmod -R a+rX,go-w -- "$id.tmp"
mv -- "$id.tmp" "$id"
echo staged
`)
	return b.String()
}

// activateScript installs the staged env file and points `current` at the
// release, printing the previous target ("" on a first deploy). The previous
// env is kept as env.prev so a failed deploy restores both together.
func activateScript(dir, id string) string {
	return fmt.Sprintf(`set -eu
cd %s
prev=$(readlink current 2>/dev/null || true)
if [ -f etc/env ]; then cp -p etc/env etc/env.prev; else rm -f etc/env.prev; fi
mv -f etc/env.next etc/env
ln -sfn %s current.tmp
mv -T current.tmp current
printf '%%s' "$prev"
`, q(dir), q("releases/"+id))
}

// revertScript points `current` back at prev and restores the env it ran with.
func revertScript(dir, prev string) string {
	return fmt.Sprintf(`set -eu
cd %s
if [ -f etc/env.prev ]; then mv -f etc/env.prev etc/env; fi
ln -sfn %s current.tmp
mv -T current.tmp current
`, q(dir), q(prev))
}

func markOKScript(dir, id string) string {
	return fmt.Sprintf("touch %s && rm -f %s", q(dir+"/releases/"+id+"/.qifa-ok"), q(dir+"/etc/env.prev"))
}

// healthScript waits until the unit is active and, if url is set, answers it.
// Without a URL, the unit must be seen active on two checks an interval apart:
// a process that crashes on startup is "active" for an instant before systemd
// notices, and one look would pass it.
func healthScript(unit, url string, timeoutSec, intervalSec int) string {
	if intervalSec < 1 {
		intervalSec = 1
	}
	probe := "ok=$((ok+1)); [ \"$ok\" -ge 2 ] && exit 0"
	if url != "" {
		probe = fmt.Sprintf("if command -v curl >/dev/null 2>&1; then curl -fsS -o /dev/null --connect-timeout 5 --max-time 5 %[1]s && exit 0; "+
			"elif command -v wget >/dev/null 2>&1; then wget -q -T 5 -O /dev/null %[1]s && exit 0; "+
			"else echo 'health check needs curl or wget on the host' >&2; exit 127; fi", q(url))
	}
	return fmt.Sprintf(`unit=%s
end=$(( $(date +%%s) + %d ))
ok=0
while :; do
  if systemctl is-active --quiet "$unit"; then
    %s
  else
    ok=0
  fi
  if [ "$(date +%%s)" -ge "$end" ]; then
    echo "$unit: $(systemctl is-active "$unit" 2>&1 || true)" >&2
    exit 1
  fi
  sleep %d
done
`, q(unit), timeoutSec, probe, intervalSec)
}

// diagnosticsScript is attached to a failed deploy, so the reason a unit did
// not come up arrives with the error instead of needing a second command.
func diagnosticsScript(unit string) string {
	return fmt.Sprintf("systemctl status %[1]s --no-pager -n 0 2>&1 || true; journalctl -u %[1]s -n 40 --no-pager 2>&1 || true", q(unit))
}

// preflightScript is read-only. It prints key=value lines the deployer checks
// on every host before changing any of them.
func preflightScript(dir string, units []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "echo arch=$(uname -m)\n")
	fmt.Fprintf(&b, "if command -v systemctl >/dev/null 2>&1; then echo systemd=yes; else echo systemd=no; fi\n")
	fmt.Fprintf(&b, "if [ -d %[1]s/releases ] && [ -w %[1]s/releases ] && [ -w %[1]s/etc ]; then echo dir=ok; else echo dir=missing; fi\n", q(dir))
	for _, u := range units {
		fmt.Fprintf(&b, "if sudo -n -l systemctl restart %[1]s >/dev/null 2>&1; then echo sudo:%[2]s=ok; else echo sudo:%[2]s=no; fi\n", q(u), u)
		fmt.Fprintf(&b, "echo unitsum:%s=$(sha256sum /etc/systemd/system/%s 2>/dev/null | cut -d' ' -f1)\n", u, q(u))
	}
	// The health check runs `systemctl is-active` as the deploy user, which
	// goes through D-Bus; a host without dbus fails it for every unit, and
	// the deploy would read that as an unhealthy app.
	b.WriteString("if systemctl show -p ActiveState --value systemd-journald.service >/dev/null 2>&1; then echo bus=ok; else echo bus=no; fi\n")
	return b.String()
}

func parseKV(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m
}
