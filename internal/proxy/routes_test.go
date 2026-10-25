package proxy

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tendant/qifa/internal/config"
	"github.com/tendant/qifa/internal/ssh"
)

// fakeDocker puts a docker on PATH that appends its arguments to calls,
// so Deploy and replays run end to end on the local host.
func fakeDocker(t *testing.T) (calls string) {
	t.Helper()
	bin := t.TempDir()
	calls = filepath.Join(bin, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + calls + "\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	return calls
}

func tlsProxy(client *ssh.Client, host string) *KamalProxy {
	return New(client, config.Proxy{
		Host: host,
		SSL:  true,
		TLS:  &config.TLS{Source: "qifa", Provider: "cloudflare"},
	}, config.ProxyBoot{})
}

func TestDeployRecordsRouteAndRenewalReplaysIt(t *testing.T) {
	calls := fakeDocker(t)
	ctx := context.Background()
	client := ssh.New(config.SSH{})

	if err := tlsProxy(client, "app.example.com").Deploy(ctx, ssh.LocalHost, Target{Service: "app", Host: "app-web-1", Port: 3000}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := tlsProxy(client, "other.example.com").Deploy(ctx, ssh.LocalHost, Target{Service: "other", Host: "other-web-1", Port: 80}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := os.Truncate(calls, 0); err != nil {
		t.Fatal(err)
	}

	services, err := ReregisterCert(ctx, client, ssh.LocalHost, "app.example.com")
	if err != nil {
		t.Fatalf("ReregisterCert: %v", err)
	}
	if !reflect.DeepEqual(services, []string{"app"}) {
		t.Fatalf("services = %v, want [app]; only routes using the renewed cert may be re-registered", services)
	}
	got, _ := os.ReadFile(calls)
	if !strings.Contains(string(got), "kamal-proxy deploy app") || !strings.Contains(string(got), "app-web-1:3000") {
		t.Fatalf("replay did not re-run the recorded deploy, docker got: %q", got)
	}
	if strings.Contains(string(got), "other") {
		t.Fatalf("replay touched an unrelated route: %q", got)
	}
}

func TestReregisterCertWithNoRecordedRoute(t *testing.T) {
	fakeDocker(t)
	services, err := ReregisterCert(context.Background(), ssh.New(config.SSH{}), ssh.LocalHost, "app.example.com")
	if err != nil || len(services) != 0 {
		t.Fatalf("got services=%v err=%v, want none and no error", services, err)
	}
}

func TestRemoveDeletesRecordedRoute(t *testing.T) {
	fakeDocker(t)
	ctx := context.Background()
	client := ssh.New(config.SSH{})
	p := tlsProxy(client, "app.example.com")
	if err := p.Deploy(ctx, ssh.LocalHost, Target{Service: "app", Host: "app-web-1", Port: 3000}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := p.Remove(ctx, ssh.LocalHost, "app"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	services, err := ReregisterCert(ctx, client, ssh.LocalHost, "app.example.com")
	if err != nil || len(services) != 0 {
		t.Fatalf("removed route was replayed: services=%v err=%v", services, err)
	}
}
