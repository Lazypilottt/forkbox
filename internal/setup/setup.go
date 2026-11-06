// Package setup installs the pinned Firecracker release and guest kernel into
// the data directory, verifying every checksum in versions.yaml.
package setup

import (
	"archive/tar"
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
	"sort"

	"github.com/Lazypilottt/forkbox/internal/datadir"
	"github.com/Lazypilottt/forkbox/internal/versions"
)

// Options configures one setup run.
type Options struct {
	DataDir  string
	Versions *versions.File
	Arch     string // Firecracker arch name, e.g. x86_64
	// Force re-downloads and re-installs even when files already verify.
	Force  bool
	Client *http.Client // nil means http.DefaultClient
	Log    io.Writer    // nil means io.Discard
}

// Installed lists the files setup placed in the data directory.
type Installed struct {
	Binaries map[string]string // name -> path
	Kernel   string
}

// ErrChecksum means a download or archive member did not match versions.yaml.
var ErrChecksum = errors.New("checksum mismatch")

// Run downloads and installs everything versions.yaml pins for opts.Arch.
// Running it again is a no-op once every file verifies.
func Run(ctx context.Context, opts Options) (*Installed, error) {
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	v := opts.Versions
	fc, kernel, err := v.ForArch(opts.Arch)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{
		datadir.Downloads(opts.DataDir),
		filepath.Dir(datadir.Bin(opts.DataDir, "x")),
		filepath.Dir(datadir.Kernel(opts.DataDir, "x")),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w (run with sudo if the data directory is root-owned)", dir, err)
		}
	}

	// 1. The release archive, kept so a forced re-install needs no network.
	archive := filepath.Join(datadir.Downloads(opts.DataDir), fc.Archive)
	if err := fetch(ctx, opts, v.ArchiveURL(fc), archive, fc.SHA256); err != nil {
		return nil, err
	}

	// 2. The binaries inside it, each checked against its own pinned checksum.
	inst := &Installed{Binaries: map[string]string{}}
	names := make([]string, 0, len(fc.Binaries))
	for name := range fc.Binaries {
		names = append(names, name)
	}
	sort.Strings(names)
	var todo []string
	for _, name := range names {
		dst := datadir.Bin(opts.DataDir, name)
		inst.Binaries[name] = dst
		if !opts.Force && fileHasSHA256(dst, fc.Binaries[name]) {
			fmt.Fprintf(opts.Log, "ok         %s (already installed, sha256 verified)\n", dst)
			continue
		}
		todo = append(todo, name)
	}
	if len(todo) > 0 {
		if err := extract(archive, v, opts, fc, todo); err != nil {
			return nil, err
		}
	}

	// 3. The guest kernel.
	inst.Kernel = datadir.Kernel(opts.DataDir, v.GuestKernel.Version)
	if err := fetch(ctx, opts, kernel.URL, inst.Kernel, kernel.SHA256); err != nil {
		return nil, err
	}
	return inst, nil
}

// FetchVerified downloads url to dst unless dst already has the wanted
// SHA-256. The file only appears at dst once it has verified. A nil client
// or log means http.DefaultClient and io.Discard.
func FetchVerified(ctx context.Context, client *http.Client, log io.Writer, url, dst, sha256 string) error {
	if client == nil {
		client = http.DefaultClient
	}
	if log == nil {
		log = io.Discard
	}
	return fetch(ctx, Options{Client: client, Log: log}, url, dst, sha256)
}

// fetch downloads url to dst unless dst already has the wanted checksum. The
// file only appears at dst once it has verified.
func fetch(ctx context.Context, opts Options, url, dst, want string) error {
	if !opts.Force && fileHasSHA256(dst, want) {
		fmt.Fprintf(opts.Log, "ok         %s (already present, sha256 verified)\n", dst)
		return nil
	}
	fmt.Fprintf(opts.Log, "download   %s\n", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := opts.Client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}
	n, err := writeVerified(dst, resp.Body, want, 0o644)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	fmt.Fprintf(opts.Log, "verified   %s (%.1f MiB, sha256 %s…)\n", dst, float64(n)/(1<<20), want[:12])
	return nil
}

// extract installs the named binaries from the release archive.
func extract(archive string, v *versions.File, opts Options, fc versions.FirecrackerArtifact, names []string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", archive, err)
	}
	defer gz.Close()

	byMember := map[string]string{}
	for _, name := range names {
		byMember[v.ArchiveMember(opts.Arch, name)] = name
	}
	tr := tar.NewReader(gz)
	for len(byMember) > 0 {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		name, ok := byMember[hdr.Name]
		if !ok || hdr.Typeflag != tar.TypeReg {
			continue
		}
		dst := datadir.Bin(opts.DataDir, name)
		if _, err := writeVerified(dst, tr, fc.Binaries[name], 0o755); err != nil {
			return fmt.Errorf("%s in %s: %w", hdr.Name, archive, err)
		}
		fmt.Fprintf(opts.Log, "installed  %s (sha256 %s…)\n", dst, fc.Binaries[name][:12])
		delete(byMember, hdr.Name)
	}
	if len(byMember) > 0 {
		missing := make([]string, 0, len(byMember))
		for m := range byMember {
			missing = append(missing, m)
		}
		sort.Strings(missing)
		return fmt.Errorf("%s does not contain %v", archive, missing)
	}
	return nil
}

// writeVerified streams r into a temporary file next to dst, checks its
// SHA-256, and renames it into place only if it matches.
func writeVerified(dst string, r io.Reader, want string, mode os.FileMode) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".part-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return n, fmt.Errorf("%w: got sha256 %s, versions.yaml pins %s", ErrChecksum, got, want)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return n, err
	}
	return n, os.Rename(tmp.Name(), dst)
}

func fileHasSHA256(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == want
}
