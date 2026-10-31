package cert

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NativeLegoVersion is the lego release a native host runs. It must match
// LegoImage's tag: the command line qifa builds is the same for both.
const NativeLegoVersion = "v5.3.1"

// legoReleaseURL is where lego's release assets live; a variable so tests
// can serve them locally.
var legoReleaseURL = "https://github.com/go-acme/lego/releases/download"

// ensureLego makes sure the pinned lego binary is on the proxy host, in the
// deploy user's own ~/.qifa/bin, and records its path. Not in the shared
// certificate directory: the proxy's account can write there, and the deploy
// user would then run whatever it put there.
//
// The binary comes from QIFA_LEGO_BINARY (a local linux binary, for hosts
// without internet on the deployer) or lego's GitHub release, checked
// against the release's checksums file and cached on the deployer.
func (m *Manager) ensureLego(ctx context.Context) error {
	if m.legoBin != "" {
		return nil
	}
	home, err := m.ssh.Run(ctx, m.proxyHost, `printf '%s' "$HOME"`)
	if err != nil || !strings.HasPrefix(home, "/") {
		return fmt.Errorf("find the deploy user's home on %s: %v", m.proxyHost, err)
	}
	remote := home + "/.qifa/bin/lego-" + NativeLegoVersion
	if _, err := m.ssh.Run(ctx, m.proxyHost, "test -x "+shellQuote(remote)); err == nil {
		m.legoBin = remote
		return nil
	}
	uname, err := m.ssh.Run(ctx, m.proxyHost, "uname -m")
	if err != nil {
		return err
	}
	arch := goArch(uname)
	local := os.Getenv("QIFA_LEGO_BINARY")
	if local == "" {
		if local, err = fetchLego(ctx, NativeLegoVersion, arch, m.out); err != nil {
			return err
		}
	}
	f, err := os.Open(local)
	if err != nil {
		return fmt.Errorf("lego binary: %w", err)
	}
	defer f.Close()
	fmt.Fprintf(m.out, "installing lego %s (linux/%s) to %s:%s\n", NativeLegoVersion, arch, m.proxyHost, remote)
	if err := m.ssh.UploadFrom(ctx, m.proxyHost, remote, f, 0o755); err != nil {
		return fmt.Errorf("upload lego: %w", err)
	}
	m.legoBin = remote
	return nil
}

// fetchLego returns a cached lego binary for linux/arch, downloading and
// verifying the release tarball when it is not cached yet.
func fetchLego(ctx context.Context, version, arch string, out io.Writer) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	bin := filepath.Join(cache, "qifa", "lego", version, "linux-"+arch, "lego")
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	plain := strings.TrimPrefix(version, "v")
	tarball := fmt.Sprintf("lego_%s_linux_%s.tar.gz", version, arch)
	sums := fmt.Sprintf("lego_%s_checksums.txt", plain)
	fmt.Fprintf(out, "downloading lego %s for linux/%s\n", version, arch)
	sumData, err := download(ctx, legoReleaseURL+"/"+version+"/"+sums)
	if err != nil {
		return "", err
	}
	want, err := checksumFor(sumData, tarball)
	if err != nil {
		return "", err
	}
	data, err := download(ctx, legoReleaseURL+"/"+version+"/"+tarball)
	if err != nil {
		return "", err
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return "", fmt.Errorf("%s: sha256 %x does not match the release checksum %s", tarball, got, want)
	}
	exe, err := extractFile(data, "lego")
	if err != nil {
		return "", fmt.Errorf("%s: %w", tarball, err)
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return "", err
	}
	tmp := bin + ".tmp"
	if err := os.WriteFile(tmp, exe, 0o755); err != nil {
		return "", err
	}
	return bin, os.Rename(tmp, bin)
}

func download(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w (set QIFA_LEGO_BINARY to a local linux lego binary to skip the download)", url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, res.Status)
	}
	return io.ReadAll(res.Body)
}

// checksumFor finds name's sha256 in a `sha256sum`-format checksums file.
func checksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("no checksum for %s in the release checksums", name)
}

// extractFile returns the contents of the regular file called name at the
// top of a tar.gz.
func extractFile(tgz []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no %s in the archive", name)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg && strings.TrimPrefix(hdr.Name, "./") == name {
			return io.ReadAll(tr)
		}
	}
}

// goArch maps `uname -m` to the architecture names lego's releases use.
func goArch(uname string) string {
	switch m := strings.TrimSpace(uname); {
	case m == "x86_64" || m == "amd64":
		return "amd64"
	case m == "aarch64" || m == "arm64":
		return "arm64"
	case strings.HasPrefix(m, "armv"):
		return "armv7"
	default:
		return m
	}
}
