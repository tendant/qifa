package ssh

import (
	"errors"
	"strings"
	"testing"
)

func TestFormatRemoteErrorUsesRemoteCommandLabel(t *testing.T) {
	err := formatRemoteError("remote command", "example-host", "echo test", errors.New("exit status 1"), "boom")
	message := err.Error()

	if !strings.Contains(message, "remote command example-host failed") {
		t.Fatalf("expected remote command label, got %q", message)
	}
	if strings.Contains(message, "ssh example-host failed") {
		t.Fatalf("did not expect ssh label, got %q", message)
	}
}
