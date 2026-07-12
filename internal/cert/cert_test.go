package cert

import (
	"reflect"
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
