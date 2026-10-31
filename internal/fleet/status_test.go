package fleet

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tendant/qifa/internal/state"
)

func TestWriteHostStatusSameLayoutBothModes(t *testing.T) {
	var native, docker bytes.Buffer
	WriteHostStatus(&native, HostStatus{
		Host:     "h1",
		Roles:    []RoleStatus{{Role: "web", Version: "v2", State: "active", Name: "qifa-app.service"}},
		Health:   "up",
		Versions: []Version{{"v3", false}, {"v2", true}, {"v1", true}},
		Rollback: "v1",
		Route:    "app.test -> 127.0.0.1:8080 (running, tls)",
		Notes:    []string{"half-staged v4.tmp"},
	})
	WriteHostStatus(&docker, HostStatus{
		Host:     "h1",
		Roles:    []RoleStatus{{Role: "web", Version: "zd2", State: "running", Name: "app-web-zd2"}, {Role: "worker"}},
		Versions: []Version{{"zd2", false}, {"zd1", false}},
		Rollback: "zd1",
	})
	for _, want := range []string{
		"\nh1\n",
		"  web      v2  active  qifa-app.service\n",
		"  health   up\n",
		"  versions v3 v2* v1*  (* passed a health check; newest first)\n",
		"  rollback v1\n",
		"  route    app.test -> 127.0.0.1:8080 (running, tls)\n",
		"  note     half-staged v4.tmp\n",
	} {
		if !strings.Contains(native.String(), want) {
			t.Errorf("native status lacks %q:\n%s", want, native.String())
		}
	}
	for _, want := range []string{
		"  web      zd2  running  app-web-zd2\n",
		"  worker   (nothing running)\n",
		"  versions zd2 zd1  (newest first)\n",
		"  rollback zd1\n",
	} {
		if !strings.Contains(docker.String(), want) {
			t.Errorf("docker status lacks %q:\n%s", want, docker.String())
		}
	}
	var failed bytes.Buffer
	WriteHostStatus(&failed, HostStatus{Host: "h2", Err: errors.New("ssh: unreachable")})
	if !strings.Contains(failed.String(), "  ERROR    ssh: unreachable") {
		t.Fatalf("error host: %s", failed.String())
	}
}

func TestWriteHistoryShowsLatestDeploymentsAndItsEvents(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	var deps []state.Deployment
	for i := 0; i < 12; i++ {
		deps = append(deps, state.Deployment{ID: string(rune('a' + i)), Service: "app", Version: "v", Status: state.StatusSucceeded, StartedAt: now.Add(time.Duration(i) * time.Minute)})
	}
	events := []state.Event{
		{DeploymentID: "l", Host: "h1", EventType: "deployed", Message: "v12\ndetail", CreatedAt: now},
		{DeploymentID: "a", Host: "h1", EventType: "deployed", Message: "old", CreatedAt: now},
	}
	var out bytes.Buffer
	WriteHistory(&out, deps, events, 10)
	s := out.String()
	if strings.Count(s, " Succeeded\n") != 10 || !strings.Contains(s, "... 2 older") {
		t.Fatalf("history:\n%s", s)
	}
	if !strings.Contains(s, "events of l:") || !strings.Contains(s, "h1 deployed v12\n") || strings.Contains(s, "deployed old") || strings.Contains(s, "detail") {
		t.Fatalf("events:\n%s", s)
	}
}
