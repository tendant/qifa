package native

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// launchd hosts (native.init: launchd) are Macs. The app runs as a
// LaunchAgent in the deploy user's login session: launchd starts it when that
// user logs in (after a reboot, the FileVault unlock does that) and restarts
// it per native.restart. Everything runs as the deploy user, so there is no
// root step, no sudoers rule and no separate service account: setup writes
// the plist, deploys bootstrap it again.
//
// Remote commands reach the user's login shell, which on a Mac is zsh; every
// script here and in scripts.go is POSIX sh, so on launchd hosts each one is
// run through /bin/sh (Deployer.run).

func (d *Deployer) launchd() bool { return d.cfg.Native.Launchd() }

// run runs a script on host, through /bin/sh where the login shell may not be
// a POSIX one.
func (d *Deployer) run(ctx context.Context, host, script string) (string, error) {
	return d.ssh.Run(ctx, host, d.sh(script))
}

func (d *Deployer) stream(ctx context.Context, host, script string, out io.Writer) error {
	return d.ssh.Stream(ctx, host, d.sh(script), out)
}

func (d *Deployer) sh(script string) string {
	if !d.launchd() {
		return script
	}
	return "/bin/sh -c " + q(script)
}

// control starts, stops or restarts a unit.
func (d *Deployer) control(ctx context.Context, host, verb, unit string) error {
	if d.launchd() {
		_, err := d.run(ctx, host, launchctlScript(verb, unit, d.logFile(unit)))
		return err
	}
	_, err := d.ssh.Sudo(ctx, host, "systemctl", verb, unit)
	return err
}

// isActive is a shell condition that holds while unit is running.
func (d *Deployer) isActive(unit string) string {
	if d.launchd() {
		return fmt.Sprintf(`launchctl print "gui/$(id -u)/"%s 2>/dev/null | grep -q 'state = running'`, q(unit))
	}
	return fmt.Sprintf("systemctl is-active --quiet %s", q(unit))
}

// unitState is a shell command printing one word for unit's state; "active"
// means running, under either init system.
func (d *Deployer) unitState(unit string) string {
	if d.launchd() {
		return fmt.Sprintf(`if %s; then echo active; else echo inactive; fi`, d.isActive(unit))
	}
	return fmt.Sprintf("systemctl is-active %s 2>/dev/null", q(unit))
}

// diagnostics is attached to a failed deploy, so the reason a unit did not
// come up arrives with the error instead of needing a second command.
func (d *Deployer) diagnostics(unit string) string {
	if d.launchd() {
		return fmt.Sprintf(`launchctl print "gui/$(id -u)/"%s 2>&1 | grep -E 'state =|pid =|last exit|runs =' || echo %s
tail -n 40 %s 2>/dev/null || true`, q(unit), q(unit+" is not loaded"), q(d.logFile(unit)))
	}
	return diagnosticsScript(unit)
}

// logFile is where launchd writes a unit's stdout and stderr.
func (d *Deployer) logFile(unit string) string { return d.cfg.Native.Dir + "/log/" + unit + ".log" }

// plistPath is the LaunchAgent's plist, under the deploy user's home: a shell
// word, quoted, for scripts that run as that user.
func plistPath(label string) string {
	return `"$HOME/Library/LaunchAgents/"` + q(label+".plist")
}

// logRotateBytes is the size at which a restart moves a unit's log aside,
// keeping one old log: macOS's own rotation (newsyslog) needs root.
const logRotateBytes = 10 << 20

// launchctlScript starts, stops or restarts a LaunchAgent. A restart boots the
// agent out and back in rather than kickstarting it, so a plist that setup has
// rewritten takes effect. bootout returns before the process has exited;
// bootstrap fails while launchd still has the old instance, hence the wait.
func launchctlScript(verb, label, logFile string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "set -eu\nlabel=%s\nplist=%s\nlog=%s\n", q(label), plistPath(label), q(logFile))
	b.WriteString(`target="gui/$(id -u)/$label"
loaded() { launchctl print "$target" >/dev/null 2>&1; }
[ -f "$plist" ] || { echo "$plist is missing — run qifa setup" >&2; exit 1; }
`)
	if verb == "stop" || verb == "restart" {
		b.WriteString(`if loaded; then launchctl bootout "$target" 2>/dev/null || true; fi
i=0
while loaded; do
  i=$((i+1))
  [ "$i" -le 120 ] || { echo "$label did not stop within 120s" >&2; exit 1; }
  sleep 1
done
`)
	}
	if verb == "start" || verb == "restart" {
		fmt.Fprintf(&b, `if [ -f "$log" ] && [ "$(wc -c < "$log")" -gt %d ]; then mv -f "$log" "$log.1"; fi
if loaded; then launchctl kickstart "$target"; else launchctl bootstrap "gui/$(id -u)" "$plist"; fi
`, logRotateBytes)
	}
	return b.String()
}

// launchdPreflight is the read-only host check for launchd, printing key=value
// lines like preflightScript.
func launchdPreflight(dir string, labels []string) string {
	var b strings.Builder
	b.WriteString("echo arch=$(uname -m)\necho os=$(uname -s)\necho user=$(id -un)\n")
	b.WriteString("if command -v launchctl >/dev/null 2>&1; then echo launchctl=yes; fi\n")
	// A LaunchAgent lives in the user's login session; with nobody logged in
	// at the console there is no gui domain to bootstrap it into.
	b.WriteString("if launchctl print \"gui/$(id -u)\" >/dev/null 2>&1; then echo gui=ok; fi\n")
	fmt.Fprintf(&b, "if [ -d %[1]s/releases ] && [ -w %[1]s/releases ] && [ -w %[1]s/etc ] && [ -w %[1]s/log ]; then echo dir=ok; else echo dir=missing; fi\n", q(dir))
	for _, l := range labels {
		fmt.Fprintf(&b, "echo unitsum:%s=$(%s 2>/dev/null | cut -d' ' -f1)\n", l, sha256Of(plistPath(l)))
	}
	fmt.Fprintf(&b, "cur=$(readlink %s/current 2>/dev/null || true)\necho current=$cur\n", q(dir))
	fmt.Fprintf(&b, "if [ -n \"$cur\" ] && [ ! -f %s/\"$cur\"/.qifa-ok ]; then echo currentok=no; fi\n", q(dir))
	// The loop's status is its last test's; a preflight that found nothing
	// half-staged still succeeded.
	fmt.Fprintf(&b, "for f in %s/releases/*.tmp; do [ -e \"$f\" ] && echo tmp=yes && break; done\n:\n", q(dir))
	return b.String()
}

// checkLaunchdHost turns launchdPreflight's output into problems.
func (d *Deployer) checkLaunchdHost(kv map[string]string) []string {
	n := d.cfg.Native
	if kv["os"] != "Darwin" || kv["launchctl"] != "yes" {
		return []string{fmt.Sprintf("native.init is launchd but this host is %s, not macOS", firstNonEmpty(kv["os"], "unknown"))}
	}
	var problems []string
	if kv["user"] != n.User {
		problems = append(problems, fmt.Sprintf("deploys connect as %s but native.user is %s: a LaunchAgent runs as the user who deploys it", kv["user"], n.User))
	}
	if kv["gui"] != "ok" {
		problems = append(problems, fmt.Sprintf("%s is not logged in on this Mac: a LaunchAgent runs in that login session (log in at the console)", n.User))
	}
	if kv["dir"] != "ok" {
		problems = append(problems, fmt.Sprintf("%s is missing or not writable by %s — run `qifa setup`", n.Dir, kv["user"]))
	}
	return problems
}

// launchdSetupScript provisions a Mac as the deploy user: the install
// directory and the plists. It needs no root and starts nothing; the first
// deploy does.
func (d *Deployer) launchdSetupScript(host string) string {
	n := d.cfg.Native
	var b strings.Builder
	fmt.Fprintf(&b, "set -eu\nuser=%s\ndir=%s\n", q(n.User), q(n.Dir))
	b.WriteString(`[ "$(uname -s)" = Darwin ] || { echo "native.init is launchd but this host is not macOS" >&2; exit 1; }
[ "$(id -un)" = "$user" ] || { echo "setup must run as $user, the user the LaunchAgent runs as (not $(id -un))" >&2; exit 1; }
mkdir -p "$dir/releases" "$dir/shared" "$dir/log" "$dir/etc"
chmod 0700 "$dir/etc"
[ -e "$dir/etc/env" ] || (umask 077 && : > "$dir/etc/env")
mkdir -p "$HOME/Library/LaunchAgents"
`)
	if n.Workdir != "" {
		// An existing workdir is left as it is: it is the app's, not qifa's.
		fmt.Fprintf(&b, "if [ ! -d %[1]s ]; then mkdir -p %[1]s; echo \"created %[2]s\"; fi\n", q(n.Workdir), n.Workdir)
	}
	for _, sd := range n.SharedDirs {
		fmt.Fprintf(&b, "mkdir -p \"$dir/shared/\"%s\n", q(sd))
	}
	for _, role := range d.rolesOn(host) {
		label := d.cfg.NativeUnitName(role)
		fmt.Fprintf(&b, `plist=%s
printf '%%s' %s > "$plist.qifa"
if cmp -s "$plist.qifa" "$plist"; then
  rm -f "$plist.qifa"
else
  chmod 0644 "$plist.qifa"
  mv -f "$plist.qifa" "$plist"
  echo "installed %s (the next deploy loads it)"
fi
`, plistPath(label), q(RenderPlist(d.plistParams(role))), label)
	}
	return b.String()
}

// launchdTeardownScript undoes launchdSetupScript, as the deploy user.
func (d *Deployer) launchdTeardownScript(host string, purge bool) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	for _, unit := range d.unitsOn(host) {
		fmt.Fprintf(&b, `launchctl bootout "gui/$(id -u)/"%[1]s >/dev/null 2>&1 || true
if [ -e %[2]s ]; then rm -f %[2]s; echo "removed %[3]s"; fi
`, q(unit), plistPath(unit), unit)
	}
	if purge {
		fmt.Fprintf(&b, "if [ -e %[1]s ]; then rm -rf -- %[1]s; echo \"deleted %[2]s\"; fi\n", q(d.cfg.Native.Dir), d.cfg.Native.Dir)
	} else {
		fmt.Fprintf(&b, "echo \"kept %s (releases and shared data); --purge deletes it\"\n", d.cfg.Native.Dir)
	}
	return b.String()
}

// launchdExecScript runs command in the app's working directory with its env,
// as the deploy user, who is the user the app runs as.
func launchdExecScript(dir, workdir, command string) string {
	return fmt.Sprintf(`if [ -f %[1]s ]; then set -a; . %[1]s; set +a; fi
cd %[2]s && exec /bin/sh -c %[3]s`, q(dir+"/etc/env"), q(workdir), q(command))
}

// PlistParams is everything a LaunchAgent plist depends on. Like a systemd
// unit it goes through `current`, so it changes only with the config.
type PlistParams struct {
	Label   string
	Dir     string
	Exec    []string
	Restart string
	Workdir string
	LogFile string
}

func (d *Deployer) plistParams(role string) PlistParams {
	label := d.cfg.NativeUnitName(role)
	p := unitParams(d.cfg, role)
	return PlistParams{
		Label:   label,
		Dir:     p.Dir,
		Exec:    p.Exec,
		Restart: p.Restart,
		Workdir: p.WorkingDir(),
		LogFile: d.logFile(label),
	}
}

// envLoader is the program launchd runs: it loads the env file the way
// systemd's EnvironmentFile=- does (optional; RenderEnv's output is valid sh)
// and then execs the app's argv untouched. The argv is passed as arguments,
// never spliced into the script, so no shell parses it. Keeping the env out
// of the plist keeps secrets out of `launchctl print` and lets a deploy
// change the env without rewriting the plist.
const envLoader = `f=$1; shift; if [ -f "$f" ]; then set -a; . "$f"; set +a; fi; exec "$@"`

// RenderArgv is the app's argv as launchd runs it: exec[0] made absolute
// under current/ when relative, as ExecStart does for systemd.
func RenderArgv(dir string, argv []string) []string {
	bin := argv[0]
	if !strings.HasPrefix(bin, "/") {
		bin = dir + "/current/" + strings.TrimPrefix(bin, "./")
	}
	return append([]string{bin}, argv[1:]...)
}

// RenderPlist renders the LaunchAgent for one role.
func RenderPlist(p PlistParams) string {
	args := append([]string{"/bin/sh", "-c", envLoader, p.Label, p.Dir + "/etc/env"}, RenderArgv(p.Dir, p.Exec)...)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Managed by qifa (qifa setup). Local edits are overwritten. -->
<plist version="1.0">
<dict>
`)
	str := func(key, value string) {
		fmt.Fprintf(&b, "\t<key>%s</key>\n\t<string>%s</string>\n", key, xmlEscape(value))
	}
	str("Label", p.Label)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range args {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("\t</array>\n")
	str("WorkingDirectory", p.Workdir)
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n" + keepAlive(p.Restart))
	// launchd's default throttle is 10s; 3s matches the systemd unit's
	// RestartSec. ExitTimeOut matches systemd's stop timeout, so a stop gives
	// the app the same 90s to finish before SIGKILL.
	b.WriteString("\t<key>ThrottleInterval</key>\n\t<integer>3</integer>\n")
	b.WriteString("\t<key>ExitTimeOut</key>\n\t<integer>90</integer>\n")
	// Standard: not Background, which macOS throttles for CPU and I/O.
	str("ProcessType", "Standard")
	str("StandardOutPath", p.LogFile)
	str("StandardErrorPath", p.LogFile)
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// keepAlive maps native.restart onto launchd's KeepAlive.
func keepAlive(restart string) string {
	switch restart {
	case "always":
		return "\t<true/>\n"
	case "no":
		return "\t<false/>\n"
	case "on-success":
		return "\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<true/>\n\t</dict>\n"
	default: // on-failure, on-abnormal, on-abort
		return "\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n"
	}
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// launchdUnitSum is the checksum preflight compares a host's plist with.
func (d *Deployer) launchdUnitSum(role string) string {
	return sha256Hex(RenderPlist(d.plistParams(role)))
}
