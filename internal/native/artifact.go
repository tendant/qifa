package native

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Package is an artifact packed for upload.
type Package struct {
	Data   []byte
	SHA256 string
	// IsDir: a directory artifact's contents become the release root; a
	// single file lands at the release root under its own Name.
	IsDir bool
	Name  string
}

// fixedTime stamps every entry, so the same input bytes produce the same
// archive and checksum on any machine. That is what lets a redeploy of a
// version reproducibly built elsewhere be recognised as the same release.
var fixedTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Pack builds a deterministic tar.gz of the artifact.
func Pack(artifact string) (*Package, error) {
	info, err := os.Lstat(artifact)
	if err != nil {
		return nil, fmt.Errorf("native.artifact: %w", err)
	}
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	tw := tar.NewWriter(gz)
	pkg := &Package{IsDir: info.IsDir(), Name: filepath.Base(artifact)}

	switch {
	case info.Mode().IsRegular():
		if err := addFile(tw, artifact, pkg.Name, info); err != nil {
			return nil, err
		}
	case info.IsDir():
		var paths []string
		err := filepath.WalkDir(artifact, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p != artifact {
				paths = append(paths, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(paths)
		if len(paths) == 0 {
			return nil, fmt.Errorf("native.artifact %s is an empty directory", artifact)
		}
		for _, p := range paths {
			rel, _ := filepath.Rel(artifact, p)
			name := filepath.ToSlash(rel)
			fi, err := os.Lstat(p)
			if err != nil {
				return nil, err
			}
			if err := addEntry(tw, p, name, fi); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("native.artifact %s must be a regular file or a directory", artifact)
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	pkg.Data = buf.Bytes()
	sum := sha256.Sum256(pkg.Data)
	pkg.SHA256 = hex.EncodeToString(sum[:])
	return pkg, nil
}

func addEntry(tw *tar.Writer, src, name string, fi fs.FileInfo) error {
	switch {
	case fi.Mode().IsRegular():
		return addFile(tw, src, name, fi)
	case fi.IsDir():
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: int64(fi.Mode().Perm()), ModTime: fixedTime, Format: tar.FormatPAX})
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777, ModTime: fixedTime, Format: tar.FormatPAX})
	default:
		return fmt.Errorf("native.artifact: %s is not a file, directory or symlink", src)
	}
}

func addFile(tw *tar.Writer, src, name string, fi fs.FileInfo) error {
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(fi.Mode().Perm()), Size: fi.Size(), ModTime: fixedTime, Format: tar.FormatPAX}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

// ExecPathInArtifact maps exec[0] (relative to the release root) to the local
// file it comes from, or "" when exec[0] is absolute or not in the artifact.
func ExecPathInArtifact(artifact string, isDir bool, bin string) string {
	if path.IsAbs(bin) {
		return ""
	}
	bin = strings.TrimPrefix(bin, "./")
	if !isDir {
		if bin == filepath.Base(artifact) {
			return artifact
		}
		return ""
	}
	return filepath.Join(artifact, filepath.FromSlash(bin))
}

// ELFArch returns the Go-style architecture of an ELF executable, or "" when
// the file is not ELF (a script, a jar), which carries no architecture.
func ELFArch(file string) (string, error) {
	f, err := elf.Open(file)
	if err != nil {
		var fe *elf.FormatError
		if errors.As(err, &fe) || errors.Is(err, io.EOF) {
			return "", nil
		}
		if _, statErr := os.Stat(file); statErr != nil {
			return "", statErr
		}
		return "", nil
	}
	defer f.Close()
	switch f.Machine {
	case elf.EM_X86_64:
		return "amd64", nil
	case elf.EM_AARCH64:
		return "arm64", nil
	case elf.EM_ARM:
		return "arm", nil
	case elf.EM_386:
		return "386", nil
	case elf.EM_RISCV:
		return "riscv64", nil
	}
	return f.Machine.String(), nil
}

// UnameArch maps `uname -m` to the Go-style names ELFArch returns.
func UnameArch(m string) string {
	switch strings.TrimSpace(m) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "i386", "i686":
		return "386"
	case "riscv64":
		return "riscv64"
	}
	if strings.HasPrefix(m, "armv") {
		return "arm"
	}
	return strings.TrimSpace(m)
}
