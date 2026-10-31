package native

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestParseInventory(t *testing.T) {
	inv := parseInventory(`current=releases/v3
release=v1 100 150
release=v3 300 0
release=v2 200 250
tmp=v4.tmp
unit=qifa-app.service active
unit=qifa-app-worker.service
health=up
`)
	if inv.Current != "v3" || inv.Health != "up" || len(inv.Tmp) != 1 {
		t.Fatalf("inventory: %+v", inv)
	}
	if got := []string{inv.Releases[0].ID, inv.Releases[1].ID, inv.Releases[2].ID}; !reflect.DeepEqual(got, []string{"v3", "v2", "v1"}) {
		t.Fatalf("not newest first: %v", got)
	}
	if inv.Units["qifa-app.service"] != "active" || inv.Units["qifa-app-worker.service"] != "unknown" {
		t.Fatalf("units: %v", inv.Units)
	}
	if empty := parseInventory(""); empty.Current != "" || len(empty.Releases) != 0 {
		t.Fatalf("empty host: %+v", empty)
	}
}

func TestRollbackTargetIsMostRecentlyHealthyNotLive(t *testing.T) {
	inv := inventory{Current: "v3", Releases: []release{
		{ID: "v3", Staged: 300, Healthy: 310},
		{ID: "v2", Staged: 200, Healthy: 210},
		// v1 was staged first but went live again most recently, then v3.
		{ID: "v1", Staged: 100, Healthy: 290},
		{ID: "v4", Staged: 400}, // never healthy
	}}
	if got := rollbackTarget(inv); got != "v1" {
		t.Fatalf("rollback target = %s, want v1", got)
	}
	if got := rollbackTarget(inventory{Current: "v1", Releases: []release{{ID: "v1", Healthy: 1}}}); got != "" {
		t.Fatalf("only the live release: target = %q", got)
	}
}

func TestPruneKeepsLiveAndRollbackTarget(t *testing.T) {
	inv := inventory{Current: "v2", Releases: []release{
		{ID: "v6", Staged: 600},
		{ID: "v5", Staged: 500},
		{ID: "v4", Staged: 400},
		{ID: "v3", Staged: 300},
		{ID: "v2", Staged: 200, Healthy: 700}, // live, old
		{ID: "v1", Staged: 100, Healthy: 650}, // rollback target, oldest
	}}
	got := pruneSet(inv, 2)
	if want := []string{"v4", "v3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("prune = %v, want %v", got, want)
	}
	if got := pruneSet(inv, 10); len(got) != 0 {
		t.Fatalf("retain 10 pruned %v", got)
	}
}

func TestParseEnvRoundTrip(t *testing.T) {
	values := map[string]string{
		"PLAIN":   "hello",
		"EMPTY":   "",
		"QUOTES":  `say "hi" and 'bye'`,
		"SLASHES": `C:\path\ and \\server \n not-a-newline`,
		"DOLLAR":  "$HOME ${X} `cmd`",
		"MULTI":   "line1\nline2\n\nline4\n",
		"EQUALS":  "a=b=c",
	}
	rendered, err := RenderEnv(values)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseEnv(string(rendered))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, values) {
		t.Fatalf("round trip:\n got %q\nwant %q", got, values)
	}
	// The empty file setup creates, and a bare assignment, parse too.
	if m, err := ParseEnv(""); err != nil || len(m) != 0 {
		t.Fatalf("empty: %v %v", m, err)
	}
	if m, err := ParseEnv("A=b\n"); err != nil || m["A"] != "b" {
		t.Fatalf("bare: %v %v", m, err)
	}
	if _, err := ParseEnv(`A="unterminated`); err == nil {
		t.Fatal("unterminated quote accepted")
	}
}

func TestDiffEnvNamesKeysOnly(t *testing.T) {
	lines := diffEnv(map[string]string{"A": "secret1", "B": "x"}, map[string]string{"A": "secret2", "C": "y"})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"~ A", "+ B", "- C"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "secret") {
		t.Fatalf("diff printed a value: %s", joined)
	}
}

func TestPhase2ScriptsParse(t *testing.T) {
	for name, s := range map[string]string{
		"inventory": inventoryScript("/opt/qifa/app", []string{"qifa-app.service"}, "http://127.0.0.1:8080/up"),
		"switch":    switchScript("/opt/qifa/app", "v1"),
		"remove":    removeReleasesScript("/opt/qifa/app", []string{"v1", "v2"}),
		"teardown":  teardownScript("/opt/qifa/app", "app", "/etc/sudoers.d/qifa-app", []string{"qifa-app.service"}, true),
		"keep":      teardownScript("/opt/qifa/app", "app", "/etc/sudoers.d/qifa-app", []string{"qifa-app.service"}, false),
	} {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s\n%s", name, err, out, s)
		}
	}
}
