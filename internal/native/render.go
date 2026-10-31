package native

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/tendant/qifa/internal/config"
	"github.com/tendant/qifa/internal/secrets"
)

// UnitParams is everything a unit file depends on. Units go through `current`,
// so they do not change from one release to the next — only when the config
// does, which is what `qifa native setup` is for.
type UnitParams struct {
	Service string
	Role    string
	User    string
	Dir     string
	Exec    []string
	Restart string
}

func unitParams(cfg *config.Config, role string) UnitParams {
	return UnitParams{
		Service: cfg.Service,
		Role:    role,
		User:    cfg.Native.User,
		Dir:     cfg.Native.Dir,
		Exec:    cfg.NativeExec(role),
		Restart: cfg.Native.Restart,
	}
}

// RenderUnit renders the systemd unit for one role.
func RenderUnit(p UnitParams) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by qifa (qifa native setup). Local edits are overwritten.\n")
	fmt.Fprintf(&b, "[Unit]\n")
	fmt.Fprintf(&b, "Description=qifa: %s (%s)\n", p.Service, p.Role)
	fmt.Fprintf(&b, "After=network-online.target\n")
	fmt.Fprintf(&b, "Wants=network-online.target\n")
	// No start rate limit. The default (5 starts in 10s) is spent by a
	// crashing release under Restart=, and then systemd refuses to start the
	// previous release qifa restores: the rollback fails and nothing runs.
	// RestartSec paces restarts; the health check decides what is healthy.
	fmt.Fprintf(&b, "StartLimitIntervalSec=0\n\n")
	fmt.Fprintf(&b, "[Service]\n")
	fmt.Fprintf(&b, "Type=exec\n")
	fmt.Fprintf(&b, "User=%s\n", p.User)
	fmt.Fprintf(&b, "Group=%s\n", p.User)
	fmt.Fprintf(&b, "WorkingDirectory=%s/current\n", p.Dir)
	fmt.Fprintf(&b, "ExecStart=%s\n", ExecStart(p.Dir, p.Exec))
	fmt.Fprintf(&b, "EnvironmentFile=-%s/etc/env\n", p.Dir)
	fmt.Fprintf(&b, "Restart=%s\n", p.Restart)
	fmt.Fprintf(&b, "RestartSec=3\n")
	fmt.Fprintf(&b, "NoNewPrivileges=true\n")
	fmt.Fprintf(&b, "PrivateTmp=true\n")
	fmt.Fprintf(&b, "ProtectSystem=strict\n")
	fmt.Fprintf(&b, "ReadWritePaths=%s/shared\n\n", p.Dir)
	fmt.Fprintf(&b, "[Install]\n")
	fmt.Fprintf(&b, "WantedBy=multi-user.target\n")
	return b.String()
}

// ExecStart renders argv for systemd's ExecStart=. The binary path is
// validated by config to need no quoting; it is made absolute under current/
// when relative. Every argument is double-quoted with backslash and quote
// escaped, and systemd's own expansions neutralised: % (specifiers) becomes
// %% and $ (environment variables) becomes $$. The result runs exactly argv,
// with no shell in between.
func ExecStart(dir string, argv []string) string {
	bin := argv[0]
	if !path.IsAbs(bin) {
		bin = dir + "/current/" + strings.TrimPrefix(bin, "./")
	}
	parts := []string{bin}
	for _, a := range argv[1:] {
		parts = append(parts, quoteExecArg(a))
	}
	return strings.Join(parts, " ")
}

func quoteExecArg(a string) string {
	a = strings.ReplaceAll(a, `\`, `\\`)
	a = strings.ReplaceAll(a, `"`, `\"`)
	a = strings.ReplaceAll(a, "%", "%%")
	a = strings.ReplaceAll(a, "$", "$$")
	return `"` + a + `"`
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RenderEnv writes values in the format systemd's EnvironmentFile= parses.
// The Docker env file (secrets.Render) takes values literally, but systemd
// strips quotes and interprets backslashes, so the same bytes would reach the
// app altered. Here every value is double-quoted, with \ " $ and ` escaped;
// newlines are kept literally, which systemd allows inside double quotes.
func RenderEnv(values map[string]string) ([]byte, error) {
	var b strings.Builder
	b.WriteString("# Managed by qifa deploy. Do not edit: the next deploy overwrites it.\n")
	for _, key := range secrets.SortedKeys(values) {
		if !envKeyRe.MatchString(key) {
			return nil, fmt.Errorf("env key %q is not a valid variable name", key)
		}
		v := values[key]
		if strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("env %s contains a NUL byte", key)
		}
		v = strings.ReplaceAll(v, `\`, `\\`)
		v = strings.ReplaceAll(v, `"`, `\"`)
		v = strings.ReplaceAll(v, "$", `\$`)
		v = strings.ReplaceAll(v, "`", "\\`")
		fmt.Fprintf(&b, "%s=\"%s\"\n", key, v)
	}
	return []byte(b.String()), nil
}

// RenderSudoers is the deploy user's whole grant: start, stop and restart of
// this service's units, by exact path and name. No wildcards (a * in sudoers
// arguments also matches spaces and further arguments) and nothing that
// writes files as root: a deploy user who could install a unit could write
// User=root into it.
func RenderSudoers(deployUser, systemctl string, units []string) string {
	var cmds []string
	for _, u := range units {
		for _, verb := range []string{"start", "stop", "restart"} {
			cmds = append(cmds, fmt.Sprintf("%s %s %s", systemctl, verb, u))
		}
	}
	return "# Managed by qifa native setup.\n" +
		fmt.Sprintf("%s ALL=(root) NOPASSWD: %s\n", deployUser, strings.Join(cmds, ", "))
}
