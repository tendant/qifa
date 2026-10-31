package cert

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func nativeManager() *Manager {
	m := New(nil, "h", io.Discard, Options{Native: true})
	m.legoBin = "/home/deploy/.qifa/bin/lego-" + NativeLegoVersion
	return m
}

func TestNativeLegoVersionMatchesImage(t *testing.T) {
	if !strings.HasSuffix(LegoImage, ":"+NativeLegoVersion) {
		t.Fatalf("LegoImage %s and NativeLegoVersion %s must name the same release", LegoImage, NativeLegoVersion)
	}
}

// Native lego runs as a binary, writing where the native proxy reads, and
// its credentials are sourced in a subshell that ends with lego.
func TestNativeLegoCommand(t *testing.T) {
	m := nativeManager()
	cmd := m.legoCommand("/dev/shm/qifa-lego-env.x", IssueOptions{Host: "app.example.com", Email: "a@b.c", Provider: "cloudflare"}, nil)
	for _, want := range []string{
		"( . '/dev/shm/qifa-lego-env.x' && '/home/deploy/.qifa/bin/lego-" + NativeLegoVersion + "' run",
		"--path '/var/lib/qifa-proxy/qifa'",
		"--dns 'cloudflare'",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("native lego command lacks %q:\n%s", want, cmd)
		}
	}
	if !strings.HasSuffix(cmd, " )") || strings.Contains(cmd, "docker") {
		t.Fatalf("native lego command:\n%s", cmd)
	}
	if out, err := exec.Command("sh", "-n", "-c", cmd).CombinedOutput(); err != nil {
		t.Fatalf("does not parse: %v %s", err, out)
	}
	// Without credentials there is nothing to source.
	if cmd := m.legoCommand("", IssueOptions{Host: "a", Email: "e", Provider: "p"}, nil); strings.Contains(cmd, "( .") || strings.Contains(cmd, ")") {
		t.Fatalf("no env file, yet: %s", cmd)
	}
}

func TestStoreCommand(t *testing.T) {
	if got := nativeManager().storeCommand("ls x"); got != "sh -c 'ls x'" {
		t.Fatalf("native store command = %s", got)
	}
	if got := testManager().storeCommand("ls x"); !strings.HasPrefix(got, "docker run --rm -v 'kamal-proxy-config':'/state' ") {
		t.Fatalf("container store command = %s", got)
	}
}

func TestEnvName(t *testing.T) {
	for k, ok := range map[string]bool{"CF_DNS_API_TOKEN": true, "a1": true, "1A": false, "A-B": false, "": false, "A B": false} {
		if envName(k) != ok {
			t.Errorf("envName(%q) = %v", k, !ok)
		}
	}
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// The download is checked against the release's checksums file; a tarball
// that does not match is refused, and a good one is cached.
func TestFetchLegoVerifiesChecksum(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	good := tarGz(t, map[string]string{"LICENSE": "x", "lego": "#!lego"})
	sum := sha256.Sum256(good)
	serve := good
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "lego_9.9.9_checksums.txt":
			fmt.Fprintf(w, "%s  lego_v9.9.9_linux_amd64.tar.gz\n%s  other.tar.gz\n", hex.EncodeToString(sum[:]), strings.Repeat("0", 64))
		case "lego_v9.9.9_linux_amd64.tar.gz":
			w.Write(serve)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := legoReleaseURL
	legoReleaseURL = srv.URL
	defer func() { legoReleaseURL = old }()

	serve = tarGz(t, map[string]string{"lego": "#!tampered"})
	if _, err := fetchLego(context.Background(), "v9.9.9", "amd64", io.Discard); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered tarball: %v", err)
	}
	serve = good
	bin, err := fetchLego(context.Background(), "v9.9.9", "amd64", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(bin); string(data) != "#!lego" {
		t.Fatalf("extracted %q", data)
	}
	srv.Close() // cached: no network needed the second time
	if again, err := fetchLego(context.Background(), "v9.9.9", "amd64", io.Discard); err != nil || again != bin {
		t.Fatalf("cache: %s %v", again, err)
	}
}

func TestGoArch(t *testing.T) {
	for in, want := range map[string]string{"x86_64": "amd64", "aarch64": "arm64", "armv7l": "armv7"} {
		if got := goArch(in); got != want {
			t.Errorf("goArch(%q) = %s", in, got)
		}
	}
}
