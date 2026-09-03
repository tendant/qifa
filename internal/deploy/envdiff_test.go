package deploy

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	got := parseEnvFile([]byte("A=1\n\n# comment\nB=has=equals\nnot-a-pair\n=novalue\n"))
	want := map[string]string{"A": "1", "B": "has=equals"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiffEnv(t *testing.T) {
	desired := map[string]string{"SAME": "1", "CHANGED": "new", "ADDED": "x"}
	live := map[string]string{"SAME": "1", "CHANGED": "old", "REMOVED": "y"}
	got := diffEnv(desired, live)
	want := []string{
		"+ ADDED  (in config, missing on host)",
		"~ CHANGED  (value differs)",
		"- REMOVED  (on host, not in config)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiffEnvInSync(t *testing.T) {
	env := map[string]string{"A": "1", "B": "2"}
	if got := diffEnv(env, map[string]string{"A": "1", "B": "2"}); len(got) != 0 {
		t.Fatalf("want no differences, got %v", got)
	}
}

// A secret's value must never reach the output, only the fact that it moved.
func TestDiffEnvNeverPrintsValues(t *testing.T) {
	for _, line := range diffEnv(
		map[string]string{"TOKEN": "super-secret-new"},
		map[string]string{"TOKEN": "super-secret-old"},
	) {
		if strings.Contains(line, "super-secret") {
			t.Fatalf("diff leaked a value: %q", line)
		}
	}
}
