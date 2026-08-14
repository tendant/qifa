package app

import (
	"reflect"
	"testing"
)

func TestParseCertFlagsDNSResolversSingleFlagCommaSeparated(t *testing.T) {
	f, err := parseCertFlags([]string{"example.com", "--dns-resolvers", "1.1.1.1:53,8.8.8.8:53"})
	if err != nil {
		t.Fatalf("parseCertFlags: %v", err)
	}
	want := []string{"1.1.1.1:53", "8.8.8.8:53"}
	if !reflect.DeepEqual(f.resolvers, want) {
		t.Fatalf("resolvers = %v, want %v", f.resolvers, want)
	}
}

func TestParseCertFlagsDNSResolversRepeatedFlag(t *testing.T) {
	f, err := parseCertFlags([]string{
		"example.com",
		"--dns-resolvers", "1.1.1.1:53",
		"--dns-resolvers", "8.8.8.8:53",
	})
	if err != nil {
		t.Fatalf("parseCertFlags: %v", err)
	}
	want := []string{"1.1.1.1:53", "8.8.8.8:53"}
	if !reflect.DeepEqual(f.resolvers, want) {
		t.Fatalf("resolvers = %v, want %v", f.resolvers, want)
	}
}

func TestParseCertFlagsDNSResolversDefaultEmpty(t *testing.T) {
	f, err := parseCertFlags([]string{"example.com"})
	if err != nil {
		t.Fatalf("parseCertFlags: %v", err)
	}
	if len(f.resolvers) != 0 {
		t.Fatalf("resolvers = %v, want empty", f.resolvers)
	}
}

func TestParseCertFlagsDNSResolversTrimsBlankEntries(t *testing.T) {
	f, err := parseCertFlags([]string{"example.com", "--dns-resolvers", " 1.1.1.1:53 ,, 8.8.8.8:53"})
	if err != nil {
		t.Fatalf("parseCertFlags: %v", err)
	}
	want := []string{"1.1.1.1:53", "8.8.8.8:53"}
	if !reflect.DeepEqual(f.resolvers, want) {
		t.Fatalf("resolvers = %v, want %v", f.resolvers, want)
	}
}

func TestParseCertFlagsLegoImage(t *testing.T) {
	f, err := parseCertFlags([]string{"example.com", "--lego-image", "goacme/lego:v5.3.1"})
	if err != nil {
		t.Fatalf("parseCertFlags: %v", err)
	}
	if f.legoImage != "goacme/lego:v5.3.1" {
		t.Fatalf("legoImage = %q, want goacme/lego:v5.3.1", f.legoImage)
	}
}

func TestParseCertFlagsExpiry(t *testing.T) {
	f, err := parseCertFlags([]string{"--expiry"})
	if err != nil {
		t.Fatalf("parseCertFlags: %v", err)
	}
	if !f.expiry {
		t.Fatal("expiry = false, want true")
	}
	if len(f.hosts) != 0 {
		t.Fatalf("hosts = %v, want none", f.hosts)
	}
}

func TestResolveLegoImagePrefersFlagOverEnv(t *testing.T) {
	t.Setenv("QIFA_LEGO_IMAGE", "goacme/lego:from-env")
	if got := resolveLegoImage("goacme/lego:from-flag"); got != "goacme/lego:from-flag" {
		t.Fatalf("resolveLegoImage = %q, want the flag value", got)
	}
}

func TestResolveLegoImageFallsBackToEnv(t *testing.T) {
	t.Setenv("QIFA_LEGO_IMAGE", "goacme/lego:from-env")
	if got := resolveLegoImage(""); got != "goacme/lego:from-env" {
		t.Fatalf("resolveLegoImage = %q, want the env value", got)
	}
}

// Empty means "let cert.New apply its pinned default" — resolveLegoImage
// must not invent a tag of its own.
func TestResolveLegoImageEmptyMeansPackageDefault(t *testing.T) {
	t.Setenv("QIFA_LEGO_IMAGE", "")
	if got := resolveLegoImage(""); got != "" {
		t.Fatalf("resolveLegoImage = %q, want empty", got)
	}
}
