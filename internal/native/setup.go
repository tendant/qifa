package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

var deployUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

// Setup provisions every host for native deploys. It is the one step that
// needs root, and the only one that writes units or sudoers: deploys run as
// the deploy user with nothing more than start/stop/restart of their own
// units. Idempotent; prints what it changed.
func (d *Deployer) Setup(ctx context.Context, deployUser string) error {
	if deployUser == "" {
		deployUser = d.cfg.SSH.User
	}
	if deployUser == "" {
		return fmt.Errorf("native setup needs the deploy user: set ssh.user in the config, or pass --deploy-user")
	}
	if !deployUserRe.MatchString(deployUser) || deployUser == "root" {
		return fmt.Errorf("deploy user %q: must be a normal account name, not root", deployUser)
	}
	for _, host := range d.hosts() {
		d.log.Printf("setup %s: service user %s, deploy user %s, dir %s", host, d.cfg.Native.User, deployUser, d.cfg.Native.Dir)
		systemctl, err := d.ssh.Run(ctx, host, "command -v systemctl")
		if err != nil || !strings.HasPrefix(systemctl, "/") {
			return fmt.Errorf("native setup on %s: systemd (systemctl) not found: %v", host, err)
		}
		script := d.setupScript(host, deployUser, systemctl)
		wrapped := fmt.Sprintf(`if [ "$(id -u)" = 0 ]; then sh -c %[1]s; else sudo -n sh -c %[1]s; fi`, q(script))
		out, err := d.ssh.Run(ctx, host, wrapped)
		if out != "" {
			for _, line := range strings.Split(out, "\n") {
				d.log.Printf("setup %s: %s", host, line)
			}
		}
		if err != nil {
			return fmt.Errorf("native setup on %s (needs root, or a user with passwordless sudo — try --ssh-user): %w", host, err)
		}
	}
	d.log.Printf("setup complete; deploy with `qifa deploy` as %s", deployUser)
	return nil
}

func (d *Deployer) setupScript(host, deployUser, systemctl string) string {
	n := d.cfg.Native
	dir := n.Dir
	var b strings.Builder
	fmt.Fprintf(&b, "set -eu\nsvc=%s\nuser=%s\ndeploy=%s\ndir=%s\n", q(d.cfg.Service), q(n.User), q(deployUser), q(dir))
	b.WriteString(`command -v systemctl >/dev/null 2>&1 || { echo "systemd is required on this host" >&2; exit 1; }
id "$deploy" >/dev/null 2>&1 || { echo "deploy user $deploy does not exist; create it first (docs/deploying.md)" >&2; exit 1; }
if ! id "$user" >/dev/null 2>&1; then
  nologin=$(command -v nologin || echo /usr/sbin/nologin)
  useradd --system --user-group --home-dir "$dir" --no-create-home --shell "$nologin" "$user"
  echo "created service user $user"
fi
install -d -m 0755 -o "$deploy" -g "$deploy" "$dir" "$dir/releases"
install -d -m 0700 -o "$deploy" -g "$deploy" "$dir/etc"
install -d -m 0750 -o "$user" -g "$user" "$dir/shared"
[ -e "$dir/etc/env" ] || install -m 0600 -o "$deploy" -g "$deploy" /dev/null "$dir/etc/env"
`)
	for _, sd := range n.SharedDirs {
		fmt.Fprintf(&b, "install -d -m 0750 -o \"$user\" -g \"$user\" \"$dir/shared/\"%s\n", q(sd))
	}
	// A Docker deploy of the same service name on this host would share the
	// lock and confuse anyone reading `qifa status`; say so, don't refuse.
	b.WriteString(`if command -v docker >/dev/null 2>&1 && [ -n "$(docker ps -aq --filter "label=qifa.service=$svc" 2>/dev/null)" ]; then
  echo "warning: this host also has Docker containers for service $svc; give the native app a different service name" >&2
fi
reload=0
`)
	units := d.unitsOn(host)
	for _, role := range d.rolesOn(host) {
		unit := d.cfg.NativeUnitName(role)
		content := RenderUnit(unitParams(d.cfg, role))
		fmt.Fprintf(&b, `printf '%%s' %s > /etc/systemd/system/.%s.qifa
if cmp -s /etc/systemd/system/.%[2]s.qifa /etc/systemd/system/%[2]s; then
  rm -f /etc/systemd/system/.%[2]s.qifa
else
  chmod 0644 /etc/systemd/system/.%[2]s.qifa
  mv -f /etc/systemd/system/.%[2]s.qifa /etc/systemd/system/%[2]s
  echo "installed unit %[2]s"
  reload=1
fi
`, q(content), unit)
	}
	b.WriteString(`[ "$reload" = 1 ] && systemctl daemon-reload
`)
	for _, unit := range units {
		fmt.Fprintf(&b, "systemctl enable --quiet %s\n", q(unit))
	}
	// sudoers.d skips file names containing '.', so the service name's dots
	// cannot appear in it.
	sudoersFile := "/etc/sudoers.d/qifa-" + strings.ReplaceAll(d.cfg.Service, ".", "_")
	fmt.Fprintf(&b, "sudoers=%s\ntmp=$(mktemp)\nprintf '%%s' %s > \"$tmp\"\n", q(sudoersFile), q(RenderSudoers(deployUser, systemctl, units)))
	b.WriteString(`if ! visudo -cqf "$tmp"; then rm -f "$tmp"; echo "generated sudoers rule failed visudo -c" >&2; exit 1; fi
chmod 0440 "$tmp"
if cmp -s "$tmp" "$sudoers"; then rm -f "$tmp"; else mv -f "$tmp" "$sudoers"; echo "installed $sudoers"; fi
if getent group systemd-journal >/dev/null 2>&1 && ! id -nG "$deploy" | tr ' ' '\n' | grep -qx systemd-journal; then
  usermod -aG systemd-journal "$deploy"
  echo "added $deploy to systemd-journal (journalctl without sudo)"
fi
`)
	return b.String()
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
