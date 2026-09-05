package deploy

import (
	"strings"
	"testing"

	"github.com/gokamal/gocart/internal/docker"
)

// Reconcile has to name what it found, because every one of these looks the
// same from outside — the site is down, or it is serving something old.
func TestDescribeMissing(t *testing.T) {
	wanted := "app-web-bb583740ded9"

	got := describeMissing(wanted, nil)
	if !strings.Contains(got, "no container "+wanted) {
		t.Errorf("nothing running should say so: %q", got)
	}

	got = describeMissing(wanted, []docker.ContainerInfo{
		{Name: "app-web-c4211e1", State: "running"},
	})
	for _, want := range []string{"app-web-c4211e1 is running", wanted} {
		if !strings.Contains(got, want) {
			t.Errorf("an older version still serving should name both; %q missing %q", got, want)
		}
	}

	// A stopped predecessor is not "serving something old" — it is nothing.
	got = describeMissing(wanted, []docker.ContainerInfo{
		{Name: "app-web-c4211e1", State: "exited"},
	})
	if !strings.Contains(got, "no container") {
		t.Errorf("a stopped predecessor should not read as running: %q", got)
	}
}
