package cert

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolverArgsDefaultsWhenEmpty(t *testing.T) {
	got := resolverArgs(nil)
	if !reflect.DeepEqual(got, DefaultDNSResolvers) {
		t.Fatalf("resolverArgs(nil) = %v, want %v", got, DefaultDNSResolvers)
	}
}

func TestResolverArgsPassesThroughOverride(t *testing.T) {
	override := []string{"9.9.9.9:53"}
	got := resolverArgs(override)
	if !reflect.DeepEqual(got, override) {
		t.Fatalf("resolverArgs(%v) = %v, want %v", override, got, override)
	}
}

func testManager() *Manager {
	return &Manager{
		legoImage:  LegoImage,
		alpineImg:  AlpineImage,
		volumeName: ProxyVolume,
		subdir:     LegoSubdir,
	}
}

// lego v5 has no `renew` subcommand; everything goes through `run`, with
// the action name ahead of the flags.
func TestLegoCommandUsesRunActionBeforeFlags(t *testing.T) {
	cmd := testManager().legoCommand("", IssueOptions{
		Host:     "reg.example.com",
		Email:    "admin@example.com",
		Provider: "cloudflare",
	}, nil)

	if strings.Contains(cmd, " renew ") {
		t.Errorf("command still uses the removed `renew` subcommand: %s", cmd)
	}
	runIdx := strings.Index(cmd, " run ")
	dnsIdx := strings.Index(cmd, "--dns ")
	if runIdx < 0 {
		t.Fatalf("command is missing the `run` action: %s", cmd)
	}
	if dnsIdx < runIdx {
		t.Errorf("--dns must come after the action name, got: %s", cmd)
	}
}

// The two flags without which real issuance fails: LE rejects the ARI
// replaces field, and recursive-resolver propagation checks time out.
func TestLegoCommandDisablesARIAndRecursiveChecks(t *testing.T) {
	cmd := testManager().legoCommand("", IssueOptions{
		Host:     "reg.example.com",
		Email:    "admin@example.com",
		Provider: "cloudflare",
	}, nil)

	for _, want := range []string{"--ari-disable", "--dns.propagation.disable-rns"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command is missing %s: %s", want, cmd)
		}
	}
}

// Renew passes --renew-days; lego v5 renamed it from --days.
func TestRenewPassesRenewDaysNotDays(t *testing.T) {
	cmd := testManager().legoCommand("", IssueOptions{
		Host:     "reg.example.com",
		Email:    "admin@example.com",
		Provider: "cloudflare",
	}, []string{"--renew-days", "30"})

	if !strings.Contains(cmd, "--renew-days") {
		t.Errorf("command is missing --renew-days: %s", cmd)
	}
	if strings.Contains(cmd, "'--days'") {
		t.Errorf("command uses the removed --days flag: %s", cmd)
	}
}

func TestLegoCommandRepeatsDomainsForSAN(t *testing.T) {
	cmd := testManager().legoCommand("", IssueOptions{
		Host:       "ci.example.com",
		ExtraHosts: []string{"ci.example.net"},
		Email:      "admin@example.com",
		Provider:   "cloudflare",
	}, nil)

	if got := strings.Count(cmd, "--domains "); got != 2 {
		t.Errorf("--domains count = %d, want 2: %s", got, cmd)
	}
}

func TestParseCertListDropsIssuerCompanions(t *testing.T) {
	got := parseCertList("reg.example.com\nreg.example.com.issuer\napp.example.com\n")
	want := []string{"app.example.com", "reg.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCertList = %v, want %v", got, want)
	}
}

func TestParseCertListEmpty(t *testing.T) {
	if got := parseCertList("  \n "); got != nil {
		t.Fatalf("parseCertList(blank) = %v, want nil", got)
	}
}
