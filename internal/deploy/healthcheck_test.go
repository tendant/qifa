package deploy

import (
	"strings"
	"testing"
)

// A minimal image (docker:cli, alpine) has no curl; busybox wget is there
// instead. Falling back keeps the probe about the app rather than about the
// tooling on the host running it.
func TestHealthcheckCommandFallsBackToWget(t *testing.T) {
	cmd := healthcheckCommand("172.18.0.7", 8090, "/readyz", 10, 5)
	for _, want := range []string{
		"command -v curl",
		"curl -fsS --connect-timeout 10 --max-time 10",
		"command -v wget",
		"wget -q -T 10 -O /dev/null",
		`"http://172.18.0.7:8090/readyz"`,
		"needs curl or wget",
		"for i in 1 2 3 4 5",
		"sleep 5",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("healthcheck command is missing %q:\n%s", want, cmd)
		}
	}
}
