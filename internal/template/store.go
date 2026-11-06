package template

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Lazypilottt/forkbox/internal/vm"
)

// Files of a template directory, <data>/templates/<id>.
const (
	MetaFile     = "template.json"
	RootfsFile   = "rootfs.ext4"
	SnapshotDir  = "snapshot" // vmstate, mem, snapshot.json (internal/vm)
	BuildLogFile = "build.log"
)

// Template is a built template, as recorded in template.json.
type Template struct {
	ID                 string             `json:"id"`
	Name               string             `json:"name"`
	Spec               Spec               `json:"spec"`
	Image              Image              `json:"image"`
	AgentSHA256        string             `json:"agent_sha256"`
	FirecrackerVersion string             `json:"firecracker_version"`
	KernelVersion      string             `json:"kernel_version"`
	VCPUs              int                `json:"vcpus"`
	MemMiB             int                `json:"mem_mib"`
	CreatedAt          time.Time          `json:"created_at"`
	Durations          map[string]float64 `json:"durations_s"`
	Sizes              map[string]Size    `json:"sizes"`
	Dir                string             `json:"-"`
}

// Size is a file's apparent size and the space it really takes (images
// and memory files are sparse).
type Size struct {
	Apparent  int64 `json:"apparent"`
	Allocated int64 `json:"allocated"`
}

// Rootfs is the template's read-only root filesystem.
func (t *Template) Rootfs() string { return filepath.Join(t.Dir, RootfsFile) }

// SnapshotDir is the template's base snapshot (vm.Restore takes it).
func (t *Template) SnapshotDir() string { return filepath.Join(t.Dir, SnapshotDir) }

// Root is <data>/templates.
func Root(dataDir string) string { return filepath.Join(dataDir, "templates") }

// Dir is a template's directory.
func Dir(dataDir, id string) string { return filepath.Join(Root(dataDir), id) }

// ErrNotFound means no template matches.
var ErrNotFound = errors.New("template not found")

// Get finds a template by id, or by name (the most recently built).
func Get(dataDir, nameOrID string) (*Template, error) {
	if strings.HasPrefix(nameOrID, "tpl-") {
		return read(Dir(dataDir, nameOrID))
	}
	all, err := List(dataDir)
	if err != nil {
		return nil, err
	}
	for _, t := range all { // newest first
		if t.Name == nameOrID {
			return t, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNotFound, nameOrID)
}

func read(dir string) (*Template, error) {
	b, err := os.ReadFile(filepath.Join(dir, MetaFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, filepath.Base(dir))
	}
	if err != nil {
		return nil, err
	}
	var t Template
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	t.Dir = dir
	return &t, nil
}

// List returns every built template, newest first. Unfinished builds
// (.build-*) are not listed.
func List(dataDir string) ([]*Template, error) {
	ents, err := os.ReadDir(Root(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Template
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "tpl-") {
			continue
		}
		t, err := read(filepath.Join(Root(dataDir), e.Name()))
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Remove deletes a template. Sandboxes already created from it keep
// working: their disks are separate reflinks, and a memory file that is
// still mapped stays alive until its last user exits.
func Remove(dataDir, nameOrID string) (*Template, error) {
	t, err := Get(dataDir, nameOrID)
	if err != nil {
		return nil, err
	}
	return t, os.RemoveAll(t.Dir)
}

func (t *Template) measure(dir string) error {
	t.Sizes = map[string]Size{}
	for key, rel := range map[string]string{
		"rootfs":   RootfsFile,
		"mem":      filepath.Join(SnapshotDir, vm.SnapshotMemFile),
		"vmstate":  filepath.Join(SnapshotDir, vm.SnapshotStateFile),
		"buildlog": BuildLogFile,
	} {
		st, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		s := Size{Apparent: st.Size()}
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			s.Allocated = sys.Blocks * 512
		}
		t.Sizes[key] = s
	}
	return nil
}
