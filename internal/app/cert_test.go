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
