package native

import (
	"fmt"
	"path"
	"strings"

	"github.com/tendant/qifa/internal/proxy"
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
func preflightScript(dir string, units []string, routed bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "echo arch=$(uname -m)\n")
	fmt.Fprintf(&b, "if command -v systemctl >/dev/null 2>&1; then echo systemd=yes; else echo systemd=no; fi\n")
	fmt.Fprintf(&b, "if [ -d %[1]s/releases ] && [ -w %[1]s/releases ] && [ -w %[1]s/etc ]; then echo dir=ok; else echo dir=missing; fi\n", q(dir))
	for _, u := range units {
		fmt.Fprintf(&b, "if sudo -n -l systemctl restart %[1]s >/dev/null 2>&1; then echo sudo:%[2]s=ok; else echo sudo:%[2]s=no; fi\n", q(u), u)
		fmt.Fprintf(&b, "echo unitsum:%s=$(sha256sum /etc/systemd/system/%s 2>/dev/null | cut -d' ' -f1)\n", u, q(u))
	}
	fmt.Fprintf(&b, "cur=$(readlink %s/current 2>/dev/null || true)\necho current=$cur\n", q(dir))
	fmt.Fprintf(&b, "if [ -n \"$cur\" ] && [ ! -f %s/\"$cur\"/.qifa-ok ]; then echo currentok=no; fi\n", q(dir))
	fmt.Fprintf(&b, "for f in %s/releases/*.tmp; do [ -e \"$f\" ] && echo tmp=yes && break; done\n", q(dir))
	// The health check runs `systemctl is-active` as the deploy user, which
	// goes through D-Bus; a host without dbus fails it for every unit, and
	// the deploy would read that as an unhealthy app.
	b.WriteString("if systemctl show -p ActiveState --value systemd-journald.service >/dev/null 2>&1; then echo bus=ok; else echo bus=no; fi\n")
	if routed {
		// Reaching the socket proves three things at once: the proxy runs,
		// it is the native one, and this user is in its group.
		fmt.Fprintf(&b, "if %s list >/dev/null 2>&1; then echo proxy=ok; else echo proxy=no; fi\n", proxy.NativeCLI())
	}
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

// inventoryScript reports a host's releases and units, one fact per line:
//
//	current=releases/<id>
//	release=<id> <staged epoch> <healthy epoch, 0 if never>
//	tmp=<name>
//	unit=<name> <state>
//	health=up|down        (only with url)
//
// The healthy time is the .qifa-ok mtime, refreshed on every deploy or
// rollback that passes, so "newest healthy" means "most recently live".
func inventoryScript(dir string, units []string, url string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cd %s 2>/dev/null || exit 0\n", q(dir))
	b.WriteString(`echo "current=$(readlink current 2>/dev/null || true)"
for d in releases/*/; do
  d=${d%/}; id=${d#releases/}
  case "$id" in *.tmp) echo "tmp=$id"; continue;; esac
  [ -d "$d" ] || continue
  st=$(stat -c %Y "$d/.qifa-checksum" 2>/dev/null || echo 0)
  ok=$(stat -c %Y "$d/.qifa-ok" 2>/dev/null || echo 0)
  echo "release=$id $st $ok"
done
`)
	for _, u := range units {
		fmt.Fprintf(&b, "echo \"unit=%s $(systemctl is-active %s 2>/dev/null)\"\n", u, q(u))
	}
	if url != "" {
		fmt.Fprintf(&b, "if curl -fsS -o /dev/null --max-time 3 %s 2>/dev/null; then echo health=up; else echo health=down; fi\n", q(url))
	}
	return b.String()
}

// switchScript points `current` at an existing release without touching the
// env file (a rollback runs the old code with today's env, as in Docker mode),
// printing the previous target.
func switchScript(dir, id string) string {
	return fmt.Sprintf(`set -eu
cd %s
[ -d %s ] || { echo "release %s is not on this host" >&2; exit 2; }
prev=$(readlink current 2>/dev/null || true)
rm -f etc/env.prev
ln -sfn %s current.tmp
mv -T current.tmp current
printf '%%s' "$prev"
`, q(dir), q("releases/"+id), id, q("releases/"+id))
}

func removeReleasesScript(dir string, ids []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "set -eu\ncd %s/releases\n", q(dir))
	for _, id := range ids {
		fmt.Fprintf(&b, "rm -rf -- %s\n", q(id))
	}
	return b.String()
}

// teardownScript runs as root: it undoes setup. With purge, the install
// directory (releases and shared data) and the service account go too.
func teardownScript(dir, user, sudoersFile string, units []string, purge bool) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	for _, u := range units {
		fmt.Fprintf(&b, "systemctl disable --now %[1]s >/dev/null 2>&1 || true\nif [ -e /etc/systemd/system/%[1]s ]; then rm -f /etc/systemd/system/%[1]s; echo \"removed unit %[2]s\"; fi\n", q(u), u)
	}
	b.WriteString("systemctl daemon-reload\n")
	fmt.Fprintf(&b, "if [ -e %[1]s ]; then rm -f %[1]s; echo \"removed %[2]s\"; fi\n", q(sudoersFile), sudoersFile)
	if purge {
		fmt.Fprintf(&b, "if [ -e %[1]s ]; then rm -rf -- %[1]s; echo \"deleted %[2]s\"; fi\n", q(dir), dir)
		fmt.Fprintf(&b, "if id %[1]s >/dev/null 2>&1; then userdel %[1]s; echo \"deleted user %[2]s\"; fi\n", q(user), user)
	} else {
		fmt.Fprintf(&b, "echo \"kept %s (releases and shared data); --purge deletes it\"\n", dir)
	}
	return b.String()
}
