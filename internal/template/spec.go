// Package template builds templates: a root filesystem made from an OCI
// image plus packages and files, booted once to run warm-up commands, and
// captured as a full base snapshot that sandboxes are restored from.
package template

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Spec is a template spec file (templates/*.yaml).
type Spec struct {
	Name string `yaml:"name" json:"name"`
	// Base is an OCI image reference, e.g. python:3.12-slim or
	// docker.io/library/python@sha256:... (a digest pins it exactly).
	Base        string            `yaml:"base" json:"base"`
	Apt         []string          `yaml:"apt,omitempty" json:"apt,omitempty"`
	Pip         []string          `yaml:"pip,omitempty" json:"pip,omitempty"`
	Files       []File            `yaml:"files,omitempty" json:"files,omitempty"`
	Env         map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	WarmUp      []string          `yaml:"warm_up,omitempty" json:"warm_up,omitempty"`
	VCPUs       int               `yaml:"vcpus,omitempty" json:"vcpus"`
	MemoryMiB   int               `yaml:"memory_mib,omitempty" json:"memory_mib"`
	DiskSizeMiB int               `yaml:"disk_size_mib,omitempty" json:"disk_size_mib"`
}

// File copies a host file (Src, relative to the spec file) into the image.
type File struct {
	Src  string `yaml:"src" json:"src"`
	Dst  string `yaml:"dst" json:"dst"`
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"` // octal, default 0644
}

// Defaults for omitted sizes.
const (
	DefaultVCPUs       = 2
	DefaultMemoryMiB   = 512
	DefaultDiskSizeMiB = 2048
)

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	pkgPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_:=<>!~,\[\]-]*$`)
	envPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ErrSpec wraps every spec error.
var ErrSpec = errors.New("invalid template spec")

func specErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrSpec, fmt.Sprintf(format, a...))
}

// LoadSpec reads and validates a spec file. Unknown keys are errors.
func LoadSpec(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseSpec(b)
}

// ParseSpec parses and validates a spec, filling in defaults.
func ParseSpec(b []byte) (*Spec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, specErr("%v", err)
	}
	if s.VCPUs == 0 {
		s.VCPUs = DefaultVCPUs
	}
	if s.MemoryMiB == 0 {
		s.MemoryMiB = DefaultMemoryMiB
	}
	if s.DiskSizeMiB == 0 {
		s.DiskSizeMiB = DefaultDiskSizeMiB
	}
	return &s, s.Validate()
}

// Validate checks the spec. Package names are passed to apt and pip as
// separate arguments, never through a shell, but are still restricted to
// the characters package names and version specifiers use.
func (s *Spec) Validate() error {
	if !namePattern.MatchString(s.Name) {
		return specErr("name %q: use 1-32 lowercase letters, digits and hyphens", s.Name)
	}
	if s.Base == "" || strings.ContainsAny(s.Base, " \t\n") {
		return specErr("base %q: give an OCI image reference such as python:3.12-slim", s.Base)
	}
	for _, p := range append(append([]string(nil), s.Apt...), s.Pip...) {
		if !pkgPattern.MatchString(p) {
			return specErr("package %q: not a package name", p)
		}
	}
	for i, f := range s.Files {
		if f.Src == "" || !filepath.IsAbs(f.Dst) {
			return specErr("files[%d]: need src and an absolute dst", i)
		}
		if _, err := f.mode(); err != nil {
			return specErr("files[%d]: mode %q: %v", i, f.Mode, err)
		}
	}
	for k, v := range s.Env {
		if !envPattern.MatchString(k) || strings.ContainsAny(v, "\x00\n") {
			return specErr("env %q: bad name or value", k)
		}
	}
	for i, c := range s.WarmUp {
		if strings.TrimSpace(c) == "" {
			return specErr("warm_up[%d] is empty", i)
		}
	}
	if s.VCPUs < 1 || s.VCPUs > 32 || (s.VCPUs > 1 && s.VCPUs%2 != 0) {
		return specErr("vcpus %d: 1 or an even number up to 32", s.VCPUs)
	}
	if s.MemoryMiB < 128 {
		return specErr("memory_mib %d: at least 128", s.MemoryMiB)
	}
	if s.DiskSizeMiB < 256 {
		return specErr("disk_size_mib %d: at least 256", s.DiskSizeMiB)
	}
	return nil
}

func (f File) mode() (os.FileMode, error) {
	if f.Mode == "" {
		return 0o644, nil
	}
	m, err := strconv.ParseUint(f.Mode, 8, 32)
	if err != nil || m > 0o7777 {
		return 0, errors.New("want octal permission bits like 0644")
	}
	return os.FileMode(m), nil
}

// EnvList is Env as sorted KEY=value entries.
func (s *Spec) EnvList() []string {
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+s.Env[k])
	}
	return out
}

// ID computes the template id: a hash of everything that determines what is
// built. That is the spec (with copied files by content, not by path),
// versions.yaml (Firecracker, kernel) and the agent binary that goes into
// the image. specDir resolves relative file sources.
func ID(s *Spec, specDir string, versionsYAML, agentBinary []byte) (string, error) {
	type fileEntry struct {
		SHA256 string `json:"sha256"`
		Dst    string `json:"dst"`
		Mode   string `json:"mode"`
	}
	canon := struct {
		Spec    Spec        `json:"spec"`
		Files   []fileEntry `json:"files"`
		Version string      `json:"versions_sha256"`
		Agent   string      `json:"agent_sha256"`
	}{Spec: *s}
	canon.Spec.Files = nil
	canon.Spec.Env = nil
	for _, f := range s.Files {
		b, err := os.ReadFile(f.path(specDir))
		if err != nil {
			return "", fmt.Errorf("template file %s: %w", f.Src, err)
		}
		m, _ := f.mode()
		canon.Files = append(canon.Files, fileEntry{sum(b), f.Dst, fmt.Sprintf("%04o", m)})
	}
	canon.Version = sum(versionsYAML)
	canon.Agent = sum(agentBinary)
	b, err := json.Marshal(struct {
		C   any      `json:"c"`
		Env []string `json:"env"`
	}{canon, s.EnvList()})
	if err != nil {
		return "", err
	}
	return "tpl-" + sum(b)[:20], nil
}

func (f File) path(specDir string) string {
	if filepath.IsAbs(f.Src) {
		return f.Src
	}
	return filepath.Join(specDir, f.Src)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
