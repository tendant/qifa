// Package version decides the identity of a deploy. It is shared by the Docker
// and native backends so a version means the same thing in both: it names the
// image or release directory, and it is what `qifa rollback <version>` takes.
package version

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Resolve decides what to call this build. The version names the image, the
// container or release directory, and the rollback target, so it must not
// quietly depend on which machine ran the deploy.
//
// In order: an explicit version (--version / QIFA_VERSION), then the git
// commit of the config's checkout, then a timestamp when there is no git at
// all. A dirty checkout is refused rather than shipped under its commit's
// name, because that tag would describe bytes nobody else can reproduce.
func Resolve(explicit string, allowDirty bool) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := strings.TrimSpace(os.Getenv("QIFA_VERSION")); env != "" {
		return env, nil
	}
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		// No git here — a tarball deploy, or a config outside any repo.
		return time.Now().UTC().Format("20060102-150405"), nil
	}
	v := strings.TrimSpace(string(out))
	dirty, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil || len(strings.TrimSpace(string(dirty))) == 0 {
		return v, nil
	}
	if allowDirty {
		return v + "-dirty", nil
	}
	return "", fmt.Errorf("the checkout has uncommitted changes, so version %s would not describe what is deployed: commit them, or pass --version <v> or --allow-dirty", v)
}
