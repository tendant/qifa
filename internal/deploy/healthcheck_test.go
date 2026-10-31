package deploy

import (
	"strings"
	"testing"
	"time"

	"github.com/tendant/qifa/internal/config"
)

// A minimal image (docker:cli, alpine) has no curl; busybox wget is there
// instead. Falling back keeps the probe about the app rather than about the
// tooling on the host running it.
func TestHealthcheckCommandFallsBackToWget(t *testing.T) {
	cmd := healthcheckCommand("172.18.0.7", 8090, "/readyz", 10, 5, 5)
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

// deploy_timeout bounds the whole check: as many attempts as fit.
func TestHealthAttemptsFitDeployTimeout(t *testing.T) {
	for _, c := range []struct {
		deploy, timeout, interval time.Duration
		want                      int
	}{
		{30 * time.Second, 5 * time.Second, 2 * time.Second, 5},
		{60 * time.Second, 5 * time.Second, 5 * time.Second, 6},
		{time.Second, 5 * time.Second, 2 * time.Second, 1},
	} {
		if got := healthAttempts(config.Healthcheck{DeployTimeout: c.deploy, Timeout: c.timeout, Interval: c.interval}); got != c.want {
			t.Errorf("healthAttempts(%v/%v/%v) = %d, want %d", c.deploy, c.timeout, c.interval, got, c.want)
		}
	}
}
