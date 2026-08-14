package cert

import (
	"bytes"
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestLegoCommandUsesConfiguredImage(t *testing.T) {
	m := testManager()
	m.legoImage = "local/lego:pinned"
	cmd := m.legoCommand("", IssueOptions{
		Host:     "reg.example.com",
		Email:    "admin@example.com",
		Provider: "cloudflare",
	}, nil)

	if !strings.Contains(cmd, "'local/lego:pinned'") {
		t.Errorf("command does not use the configured image: %s", cmd)
	}
}

// The default must be an exact tag — a floating :latest is what broke
// every issuance when lego shipped v5.
func TestDefaultLegoImageIsPinned(t *testing.T) {
	if strings.HasSuffix(LegoImage, ":latest") || !strings.Contains(LegoImage, ":") {
		t.Fatalf("LegoImage = %q, want an exact version tag", LegoImage)
	}
	if strings.HasSuffix(AlpineImage, ":latest") || !strings.Contains(AlpineImage, ":") {
		t.Fatalf("AlpineImage = %q, want an exact version tag", AlpineImage)
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

// certPEM builds a self-signed cert with the given SANs, plus an
// optional trailing block, mimicking lego's leaf-then-issuer bundle.
func certPEM(t *testing.T, names []string, notAfter time.Time, extraBlocks int) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(crand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(crand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	var buf bytes.Buffer
	for i := 0; i <= extraBlocks; i++ {
		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
			t.Fatalf("encode pem: %v", err)
		}
	}
	return buf.Bytes()
}

func TestDNSNamesFromPEMReadsLeafSANs(t *testing.T) {
	want := []string{"tripmemo.ai", "www.tripmemo.ai"}
	got, err := dnsNamesFromPEM(certPEM(t, want, time.Now().Add(24*time.Hour), 0))
	if err != nil {
		t.Fatalf("dnsNamesFromPEM: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dnsNamesFromPEM = %v, want %v", got, want)
	}
}

// lego writes the leaf first, then the issuer chain — take the leaf.
func TestDNSNamesFromPEMIgnoresIssuerChain(t *testing.T) {
	want := []string{"ci.memochat.ai", "ci.tripmemo.ai"}
	got, err := dnsNamesFromPEM(certPEM(t, want, time.Now().Add(24*time.Hour), 1))
	if err != nil {
		t.Fatalf("dnsNamesFromPEM: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dnsNamesFromPEM = %v, want %v", got, want)
	}
}

// ssh.Client.Run trims trailing whitespace, so the PEM arrives without
// its final newline — pem.Decode needs it back.
func TestDNSNamesFromPEMHandlesTrimmedTrailingNewline(t *testing.T) {
	pemBytes := certPEM(t, []string{"reg.memochat.ai"}, time.Now().Add(24*time.Hour), 0)
	got, err := dnsNamesFromPEM(bytes.TrimSpace(pemBytes))
	if err != nil {
		t.Fatalf("dnsNamesFromPEM on trimmed input: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"reg.memochat.ai"}) {
		t.Fatalf("dnsNamesFromPEM = %v", got)
	}
}

func TestDNSNamesFromPEMRejectsNonCert(t *testing.T) {
	if _, err := dnsNamesFromPEM([]byte("not a pem file")); err == nil {
		t.Fatal("expected an error for input with no CERTIFICATE block")
	}
}

func TestLeafFromPEMReadsNotAfter(t *testing.T) {
	want := time.Now().Add(45 * 24 * time.Hour).Truncate(time.Second)
	leaf, err := leafFromPEM(certPEM(t, []string{"weilabs.ai"}, want, 0))
	if err != nil {
		t.Fatalf("leafFromPEM: %v", err)
	}
	if !leaf.NotAfter.Equal(want.UTC()) {
		t.Fatalf("NotAfter = %v, want %v", leaf.NotAfter, want.UTC())
	}
	if days := (CertInfo{NotAfter: leaf.NotAfter}).DaysLeft(time.Now()); days != 44 && days != 45 {
		t.Fatalf("DaysLeft = %d, want 44 or 45", days)
	}
}

func TestCertInfoDaysLeftIsNegativeWhenExpired(t *testing.T) {
	info := CertInfo{NotAfter: time.Now().Add(-48 * time.Hour)}
	if got := info.DaysLeft(time.Now()); got >= 0 {
		t.Fatalf("DaysLeft = %d, want negative for an expired cert", got)
	}
}

// A cert that died a few hours ago must not read as 0 days left —
// truncation toward zero would report it as still having today.
func TestCertInfoDaysLeftFloorsForCertExpiredToday(t *testing.T) {
	info := CertInfo{NotAfter: time.Now().Add(-8 * time.Hour)}
	if got := info.DaysLeft(time.Now()); got != -1 {
		t.Fatalf("DaysLeft = %d, want -1 for a cert that expired earlier today", got)
	}
}

func TestSanExtras(t *testing.T) {
	tests := []struct {
		name    string
		primary string
		names   []string
		want    []string
	}{
		{"primary first", "tripmemo.ai", []string{"tripmemo.ai", "www.tripmemo.ai"}, []string{"www.tripmemo.ai"}},
		{"primary in middle", "b.example.com", []string{"a.example.com", "b.example.com"}, []string{"a.example.com"}},
		{"single name", "reg.example.com", []string{"reg.example.com"}, nil},
		{"case-insensitive primary", "Reg.Example.com", []string{"reg.example.com", "alt.example.com"}, []string{"alt.example.com"}},
		{"duplicates collapse", "a.example.com", []string{"a.example.com", "b.example.com", "B.example.com"}, []string{"b.example.com"}},
		{"no names", "a.example.com", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanExtras(tt.primary, tt.names); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("sanExtras(%q, %v) = %v, want %v", tt.primary, tt.names, got, tt.want)
			}
		})
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
