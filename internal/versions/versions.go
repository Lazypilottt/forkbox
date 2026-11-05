// Package versions loads and validates versions.yaml, the file that pins
// Firecracker, the jailer, the guest kernel and the base rootfs.
package versions

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	forkbox "github.com/Lazypilottt/forkbox"
	"go.yaml.in/yaml/v3"
)

// File is the parsed versions.yaml.
type File struct {
	Schema      int         `yaml:"schema"`
	Firecracker Firecracker `yaml:"firecracker"`
	GuestKernel Download    `yaml:"guest_kernel"`
	BaseRootfs  Download    `yaml:"base_rootfs"`
}

// Firecracker pins one Firecracker release; the jailer comes from the same archive.
type Firecracker struct {
	Version    string                         `yaml:"version"`
	ReleaseURL string                         `yaml:"release_url"`
	Artifacts  map[string]FirecrackerArtifact `yaml:"artifacts"`
}

// FirecrackerArtifact is the release archive for one architecture.
type FirecrackerArtifact struct {
	Archive string `yaml:"archive"`
	SHA256  string `yaml:"sha256"`
	// Binaries maps a binary name (firecracker, jailer, ...) to its SHA-256.
	Binaries map[string]string `yaml:"binaries"`
}

// Download pins a single downloadable file per architecture.
type Download struct {
	Version   string              `yaml:"version"`
	Source    string              `yaml:"source"`
	Artifacts map[string]Artifact `yaml:"artifacts"`
}

// Artifact is one downloadable file and its checksum.
type Artifact struct {
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
}

// RequiredBinaries must be pinned for every architecture.
var RequiredBinaries = []string{"firecracker", "jailer"}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Embedded returns the versions.yaml compiled into this binary.
func Embedded() (*File, error) { return Parse(forkbox.VersionsYAML) }

// Load reads path, or the embedded file when path is empty.
func Load(path string) (*File, error) {
	if path == "" {
		return Embedded()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Parse decodes and validates a versions.yaml document. Unknown keys are errors.
func Parse(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse versions.yaml: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Validate checks that every pin is complete and well formed.
func (f *File) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if f.Schema != 1 {
		bad("schema: want 1, got %d", f.Schema)
	}
	fc := f.Firecracker
	if !strings.HasPrefix(fc.Version, "v") {
		bad("firecracker.version: want a release tag like v1.2.3, got %q", fc.Version)
	}
	if !strings.HasPrefix(fc.ReleaseURL, "https://") {
		bad("firecracker.release_url: must be https, got %q", fc.ReleaseURL)
	}
	if len(fc.Artifacts) == 0 {
		bad("firecracker.artifacts: no architectures pinned")
	}
	for _, arch := range sortedKeys(fc.Artifacts) {
		a := fc.Artifacts[arch]
		if a.Archive == "" {
			bad("firecracker.artifacts.%s.archive: missing", arch)
		}
		if !sha256Re.MatchString(a.SHA256) {
			bad("firecracker.artifacts.%s.sha256: not a lowercase hex SHA-256", arch)
		}
		for _, b := range RequiredBinaries {
			if _, ok := a.Binaries[b]; !ok {
				bad("firecracker.artifacts.%s.binaries.%s: missing", arch, b)
			}
		}
		for _, b := range sortedKeys(a.Binaries) {
			if !sha256Re.MatchString(a.Binaries[b]) {
				bad("firecracker.artifacts.%s.binaries.%s: not a lowercase hex SHA-256", arch, b)
			}
		}
	}
	for name, d := range map[string]Download{"guest_kernel": f.GuestKernel, "base_rootfs": f.BaseRootfs} {
		if d.Version == "" {
			bad("%s.version: missing", name)
		}
		if len(d.Artifacts) == 0 {
			bad("%s.artifacts: no architectures pinned", name)
		}
		for _, arch := range sortedKeys(d.Artifacts) {
			a := d.Artifacts[arch]
			if !strings.HasPrefix(a.URL, "https://") {
				bad("%s.artifacts.%s.url: must be https, got %q", name, arch, a.URL)
			}
			if !sha256Re.MatchString(a.SHA256) {
				bad("%s.artifacts.%s.sha256: not a lowercase hex SHA-256", name, arch)
			}
		}
	}
	if len(errs) > 0 {
		sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
		return fmt.Errorf("invalid versions.yaml: %w", errors.Join(errs...))
	}
	return nil
}

// Arch maps a Go GOARCH to the architecture name Firecracker uses.
func Arch(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "x86_64", nil
	case "arm64":
		return "aarch64", nil
	}
	return "", fmt.Errorf("firecracker does not support GOARCH %q", goarch)
}

// PinnedArches lists the architectures that have every artifact pinned.
func (f *File) PinnedArches() []string {
	var out []string
	for _, arch := range sortedKeys(f.Firecracker.Artifacts) {
		if _, ok := f.GuestKernel.Artifacts[arch]; ok {
			out = append(out, arch)
		}
	}
	return out
}

// ForArch returns the Firecracker archive and guest kernel pinned for arch.
func (f *File) ForArch(arch string) (FirecrackerArtifact, Artifact, error) {
	fc, ok1 := f.Firecracker.Artifacts[arch]
	k, ok2 := f.GuestKernel.Artifacts[arch]
	if !ok1 || !ok2 {
		return FirecrackerArtifact{}, Artifact{}, fmt.Errorf(
			"versions.yaml pins no Firecracker release and guest kernel for %s (pinned: %s)",
			arch, strings.Join(f.PinnedArches(), ", "))
	}
	return fc, k, nil
}

// ArchiveURL is the download URL of the release archive for arch.
func (f *File) ArchiveURL(a FirecrackerArtifact) string {
	return strings.TrimSuffix(f.Firecracker.ReleaseURL, "/") + "/" + a.Archive
}

// ArchiveMember is the path of a binary inside the release archive, e.g.
// release-v1.17.0-x86_64/jailer-v1.17.0-x86_64.
func (f *File) ArchiveMember(arch, binary string) string {
	v := f.Firecracker.Version
	return fmt.Sprintf("release-%s-%s/%s-%s-%s", v, arch, binary, v, arch)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
