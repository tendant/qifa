package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Settings that mean the same thing in Docker and native mode live in one
// place. The keys they replaced still load: Load moves each into its new place
// and records a warning (Config.Warnings), and the accessors below fall back
// to them, so a Config built in code without Load behaves the same.
//
//	healthcheck            was proxy.healthcheck (Docker), native.healthcheck
//	servers.<role>.app_port, proxy.app_port   native.port was the native one
//	prune.retain           was prune.retain_containers, native.retain
//	servers.<role>.cmd     was servers.<role>.exec and native.exec (native)

// Health-check defaults, for both modes.
const (
	DefaultHealthInterval      = 2 * time.Second
	DefaultHealthTimeout       = 5 * time.Second
	DefaultHealthDeployTimeout = 30 * time.Second
	DefaultRetain              = 5
)

// Health returns the health check with defaults applied: Timeout bounds one
// attempt, DeployTimeout the whole wait for a new release or container.
func (c *Config) Health() Healthcheck {
	h := c.Healthcheck
	if h.isZero() {
		switch {
		case c.Native != nil && !c.Native.Healthcheck.isZero():
			h = fromNativeHealth(c.Native.Healthcheck)
		default:
			h = c.Proxy.Healthcheck
		}
	}
	if h.Interval == 0 {
		h.Interval = DefaultHealthInterval
	}
	if h.Timeout == 0 {
		h.Timeout = DefaultHealthTimeout
	}
	if h.DeployTimeout == 0 {
		h.DeployTimeout = DefaultHealthDeployTimeout
	}
	return h
}

// fromNativeHealth maps the old native.healthcheck, whose timeout was the
// whole wait, onto the unified meaning.
func fromNativeHealth(old Healthcheck) Healthcheck {
	h := old
	h.DeployTimeout = old.Timeout
	h.Timeout = 0
	return h
}

func (h Healthcheck) isZero() bool { return h == Healthcheck{} }

// IsZero lets yaml omit an unset health check.
func (h Healthcheck) IsZero() bool { return h.isZero() }

// AppPort is the port a role's app listens on: the role's app_port, else, for
// the role the proxy serves, proxy.app_port. In Docker mode a published host
// port stands in when app_port is unset, as it always has.
func (c *Config) AppPort(role string) int {
	s := c.Servers[role]
	if s.AppPort > 0 {
		return s.AppPort
	}
	if c.Native != nil {
		if role == c.NativeProxyRole() {
			if c.Proxy.AppPort > 0 {
				return c.Proxy.AppPort
			}
			return c.Native.Port
		}
		return 0
	}
	if s.Port > 0 {
		return s.Port
	}
	if role == "web" {
		return c.Proxy.AppPort
	}
	return 0
}

// Retain is how many previous versions (stopped containers, or releases) to
// keep per host.
func (c *Config) Retain() int {
	switch {
	case c.Prune.Retain > 0:
		return c.Prune.Retain
	case c.Prune.RetainContainers > 0:
		return c.Prune.RetainContainers
	case c.Native != nil && c.Native.Retain > 0:
		return c.Native.Retain
	}
	return DefaultRetain
}

// Command is servers.<role>.cmd: a string (passed to a shell in Docker mode,
// split into words in native mode) or a list of arguments.
type Command struct {
	Shell string
	Argv  []string
}

func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&c.Shell)
	case yaml.SequenceNode:
		return n.Decode(&c.Argv)
	}
	return fmt.Errorf("line %d: cmd must be a string or a list of strings", n.Line)
}

func (c Command) MarshalYAML() (any, error) {
	if len(c.Argv) > 0 {
		return c.Argv, nil
	}
	return c.Shell, nil
}

// IsZero lets yaml omit an empty command.
func (c Command) IsZero() bool { return c.Shell == "" && len(c.Argv) == 0 }

// ShellString renders the command for a shell: the string as given, or the
// list with each argument quoted.
func (c Command) ShellString() string {
	if len(c.Argv) == 0 {
		return c.Shell
	}
	quoted := make([]string, len(c.Argv))
	for i, a := range c.Argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
	}
	return strings.Join(quoted, " ")
}

// Args returns the command as arguments: the list, or the string split into
// words with shell quoting rules. A string that needs a shell for anything
// more (variables, globs, pipes, redirects, ;) is refused rather than run
// differently than it reads.
func (c Command) Args() ([]string, error) {
	if len(c.Argv) > 0 || c.Shell == "" {
		return c.Argv, nil
	}
	return SplitWords(c.Shell)
}

// SplitWords splits s like a POSIX shell splits a simple command, honouring
// single and double quotes and backslashes, and refuses anything that would
// need a shell to mean what it says.
func SplitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case ch == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case ch == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '$' || s[i] == '`' {
					return nil, fmt.Errorf("%q needs a shell ($ or ` inside quotes); write cmd as a list", s)
				}
				if s[i] == '\\' && i+1 < len(s) && strings.ContainsRune(`"\`, rune(s[i+1])) {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New("unterminated double quote")
			}
			inWord = true
		case ch == '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			}
			inWord = true
		case strings.IndexByte("$`|&;<>()*?[]{}~#", ch) >= 0:
			return nil, fmt.Errorf("%q needs a shell (%q); write cmd as a list of arguments", s, string(ch))
		default:
			cur.WriteByte(ch)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// migrate moves replaced keys into their unified place, recording a warning
// for each, and refuses a config that sets a setting in both places.
func migrate(c *Config) error {
	warn := func(old, now string) {
		c.Warnings = append(c.Warnings, fmt.Sprintf("%s is deprecated: use %s (it means the same)", old, now))
	}
	// healthcheck
	if !c.Proxy.Healthcheck.isZero() {
		if !c.Healthcheck.isZero() {
			return errors.New("config.proxy.healthcheck and config.healthcheck are both set: keep healthcheck only")
		}
		c.Healthcheck, c.Proxy.Healthcheck = c.Proxy.Healthcheck, Healthcheck{}
		warn("proxy.healthcheck", "healthcheck")
	}
	if c.Native != nil && !c.Native.Healthcheck.isZero() {
		if !c.Healthcheck.isZero() {
			return errors.New("config.native.healthcheck and config.healthcheck are both set: keep healthcheck only")
		}
		c.Healthcheck, c.Native.Healthcheck = fromNativeHealth(c.Native.Healthcheck), Healthcheck{}
		c.Warnings = append(c.Warnings, "native.healthcheck is deprecated: use healthcheck (its timeout, the whole wait, is now healthcheck.deploy_timeout)")
	}
	// native.port
	if c.Native != nil && c.Native.Port != 0 {
		if c.Proxy.AppPort != 0 && c.Proxy.AppPort != c.Native.Port {
			return fmt.Errorf("config.native.port (%d) and config.proxy.app_port (%d) disagree: keep proxy.app_port (or the role's app_port) only", c.Native.Port, c.Proxy.AppPort)
		}
		if role := c.NativeProxyRole(); role != "" && c.Servers[role].AppPort == 0 {
			s := c.Servers[role]
			s.AppPort = c.Native.Port
			c.Servers[role] = s
		}
		c.Native.Port = 0
		warn("native.port", "app_port on the role (servers.<role>.app_port)")
	}
	// retention
	if c.Prune.RetainContainers != 0 {
		if c.Prune.Retain != 0 {
			return errors.New("config.prune.retain_containers and config.prune.retain are both set: keep prune.retain only")
		}
		c.Prune.Retain, c.Prune.RetainContainers = c.Prune.RetainContainers, 0
		warn("prune.retain_containers", "prune.retain")
	}
	if c.Native != nil && c.Native.Retain != 0 {
		if c.Prune.Retain != 0 {
			return errors.New("config.native.retain and config.prune.retain are both set: keep prune.retain only")
		}
		c.Prune.Retain, c.Native.Retain = c.Native.Retain, 0
		warn("native.retain", "prune.retain")
	}
	// command
	for role, s := range c.Servers {
		if len(s.Exec) > 0 {
			if !s.Cmd.IsZero() {
				return fmt.Errorf("config.servers.%s.exec and config.servers.%s.cmd are both set: keep cmd only", role, role)
			}
			s.Cmd, s.Exec = Command{Argv: s.Exec}, nil
			c.Servers[role] = s
			warn("servers."+role+".exec", "servers."+role+".cmd")
		}
	}
	if c.Native != nil && len(c.Native.Exec) > 0 {
		for role, s := range c.Servers {
			if s.Cmd.IsZero() {
				s.Cmd = Command{Argv: c.Native.Exec}
				c.Servers[role] = s
			}
		}
		c.Native.Exec = nil
		warn("native.exec", "servers.<role>.cmd")
	}
	return nil
}

// ProxyRoute is the proxy block with the unified health check folded in, as
// the kamal-proxy client needs it: health flags from Health(), and
// --deploy-timeout from healthcheck.deploy_timeout unless proxy.deploy_timeout
// overrides it for kamal-proxy alone.
func (c *Config) ProxyRoute() Proxy {
	p := c.Proxy
	p.Healthcheck = c.Health()
	if p.DeployTimeout == 0 {
		p.DeployTimeout = p.Healthcheck.DeployTimeout
	}
	if p.DrainTimeout == 0 {
		p.DrainTimeout = 30 * time.Second
	}
	if p.TargetTimeout == 0 {
		p.TargetTimeout = 30 * time.Second
	}
	return p
}
