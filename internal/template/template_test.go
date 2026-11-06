package template

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agent"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const goodSpec = `name: python
base: python:3.12-slim
apt: [git, build-essential]
pip: [numpy, "pytest>=8"]
env: {PYTHONUNBUFFERED: "1"}
warm_up: ["python -c 'import numpy'"]
memory_mib: 1024
`

func TestParseSpec(t *testing.T) {
	s, err := ParseSpec([]byte(goodSpec))
	must(t, err)
	if s.VCPUs != DefaultVCPUs || s.MemoryMiB != 1024 || s.DiskSizeMiB != DefaultDiskSizeMiB {
		t.Errorf("defaults: %+v", s)
	}
	bad := map[string]string{
		"unknown key":   "name: x\nbase: debian\nfoo: 1\n",
		"no base":       "name: x\n",
		"bad name":      "name: X_Y\nbase: debian\n",
		"shell in pkg":  "name: x\nbase: debian\napt: ['git; rm -rf /']\n",
		"option as pkg": "name: x\nbase: debian\napt: ['--allow-unauthenticated']\n",
		"relative dst":  "name: x\nbase: debian\nfiles: [{src: a, dst: etc/a}]\n",
		"bad mode":      "name: x\nbase: debian\nfiles: [{src: a, dst: /a, mode: '999'}]\n",
		"odd vcpus":     "name: x\nbase: debian\nvcpus: 3\n",
		"tiny memory":   "name: x\nbase: debian\nmemory_mib: 64\n",
		"bad env":       "name: x\nbase: debian\nenv: {'A B': x}\n",
		"empty warm_up": "name: x\nbase: debian\nwarm_up: ['  ']\n",
	}
	for name, y := range bad {
		if _, err := ParseSpec([]byte(y)); !errors.Is(err, ErrSpec) {
			t.Errorf("%s: got %v, want ErrSpec", name, err)
		}
	}
}

func TestIDIsContentAddressed(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "motd"), []byte("hello"), 0o644))
	spec := func(y string) *Spec {
		s, err := ParseSpec([]byte(y))
		must(t, err)
		return s
	}
	base := "name: x\nbase: debian:12\nfiles: [{src: motd, dst: /etc/motd}]\nenv: {A: '1', B: '2'}\n"
	id := func(s *Spec, vers, agentBin string) string {
		i, err := ID(s, dir, []byte(vers), []byte(agentBin))
		must(t, err)
		return i
	}
	a := id(spec(base), "v1", "agent")
	if !strings.HasPrefix(a, "tpl-") || len(a) != 24 {
		t.Errorf("id %q", a)
	}
	if b := id(spec("env: {B: '2', A: '1'}\nfiles: [{src: motd, dst: /etc/motd}]\nbase: debian:12\nname: x\n"), "v1", "agent"); b != a {
		t.Errorf("key order changed the id: %s vs %s", a, b)
	}
	for name, other := range map[string]string{
		"versions.yaml": id(spec(base), "v2", "agent"),
		"agent":         id(spec(base), "v1", "agent2"),
		"memory":        id(spec(base+"memory_mib: 1024\n"), "v1", "agent"),
		"warm_up":       id(spec(base+"warm_up: [true]\n"), "v1", "agent"),
	} {
		if other == a {
			t.Errorf("changing %s kept the id", name)
		}
	}
	must(t, os.WriteFile(filepath.Join(dir, "motd"), []byte("changed"), 0o644))
	if id(spec(base), "v1", "agent") == a {
		t.Errorf("changing a copied file's content kept the id")
	}
}

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func layer(t *testing.T, entries ...entry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, ModTime: time.Unix(1700000000, 0)}
		if h.Mode == 0 {
			h.Mode = 0o644
			if e.typ == tar.TypeDir {
				h.Mode = 0o755
			}
		}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		must(t, tw.WriteHeader(h))
		if e.typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	must(t, tw.Close())
	return static.NewLayer(buf.Bytes(), types.DockerLayer)
}

func image(t *testing.T, layers ...v1.Layer) *Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	must(t, err)
	img, err = mutate.Config(img, v1.Config{Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"}, WorkingDir: "/app"})
	must(t, err)
	im, err := imageFrom("test:latest", img)
	must(t, err)
	return im
}

func TestExtractAppliesLayers(t *testing.T) {
	lower := layer(t,
		entry{name: "etc/", typ: tar.TypeDir},
		entry{name: "etc/a", body: "v1", typ: tar.TypeReg},
		entry{name: "bin/tool", body: "#!/bin/sh\n", typ: tar.TypeReg, mode: 0o4755},
		entry{name: "usr/lib/x", body: "lib", typ: tar.TypeReg},
		entry{name: "lib", link: "usr/lib", typ: tar.TypeSymlink},
		entry{name: "old", body: "gone", typ: tar.TypeReg},
		entry{name: "opaque/f1", body: "hidden", typ: tar.TypeReg},
		entry{name: "dev/null", typ: tar.TypeChar},
		entry{name: "ro/", typ: tar.TypeDir, mode: 0o555},
		entry{name: "ro/f", body: "in a read-only dir", typ: tar.TypeReg},
	)
	upper := layer(t,
		entry{name: ".wh.old", typ: tar.TypeReg},
		entry{name: "opaque/.wh..wh..opq", typ: tar.TypeReg},
		entry{name: "opaque/f2", body: "visible", typ: tar.TypeReg},
		entry{name: "etc/a", body: "v2", typ: tar.TypeReg},
		entry{name: "etc/b", link: "etc/a", typ: tar.TypeLink},
	)
	img := image(t, lower, upper)
	if img.WorkDir != "/app" || len(img.Env) != 2 {
		t.Errorf("config: %+v", img)
	}
	dst := filepath.Join(t.TempDir(), "root")
	st, err := extractImage(img, dst, false)
	must(t, err)

	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(dst, p))
		if err != nil {
			return "<" + err.Error() + ">"
		}
		return string(b)
	}
	for p, want := range map[string]string{"etc/a": "v2", "etc/b": "v2", "usr/lib/x": "lib", "lib/x": "lib", "opaque/f2": "visible", "ro/f": "in a read-only dir"} {
		if got := read(p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	for _, p := range []string{"old", "opaque/f1", "dev/null"} {
		if _, err := os.Lstat(filepath.Join(dst, p)); !os.IsNotExist(err) {
			t.Errorf("%s should not exist (err=%v)", p, err)
		}
	}
	a, _ := os.Stat(filepath.Join(dst, "etc/a"))
	b, _ := os.Stat(filepath.Join(dst, "etc/b"))
	if !os.SameFile(a, b) {
		t.Errorf("etc/b is not a hard link of etc/a")
	}
	if fi, _ := os.Stat(filepath.Join(dst, "bin/tool")); fi.Mode()&os.ModeSetuid == 0 || fi.Mode().Perm() != 0o755 {
		t.Errorf("bin/tool mode = %v", fi.Mode())
	}
	if l, _ := os.Readlink(filepath.Join(dst, "lib")); l != "usr/lib" {
		t.Errorf("lib -> %q", l)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "ro")); fi.Mode().Perm() != 0o555 {
		t.Errorf("ro mode = %v", fi.Mode().Perm())
	}
	if st.Hardlinks != 1 || len(st.Skipped) != 1 {
		t.Errorf("stats = %+v", st)
	}
	os.Chmod(filepath.Join(dst, "ro"), 0o755) // let TempDir clean up
}

func TestExtractStaysInsideRoot(t *testing.T) {
	outside := t.TempDir()
	evil := image(t,
		layer(t, entry{name: "escape", link: outside, typ: tar.TypeSymlink}),
		layer(t, entry{name: "escape/pwned", body: "x", typ: tar.TypeReg}),
	)
	dst := filepath.Join(t.TempDir(), "root")
	if _, err := extractImage(evil, dst, false); err == nil {
		t.Errorf("extraction through an absolute symlink succeeded")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); !os.IsNotExist(err) {
		t.Fatalf("a file was written outside the root")
	}
	rel := image(t,
		layer(t, entry{name: "up", link: "../../..", typ: tar.TypeSymlink}),
		layer(t, entry{name: "up/pwned", body: "x", typ: tar.TypeReg}),
	)
	dst2 := filepath.Join(t.TempDir(), "root")
	extractImage(rel, dst2, false) // the link is dropped or the write refused
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst2), "pwned")); !os.IsNotExist(err) {
		t.Fatalf("a file was written outside the root via a relative link")
	}
}

func TestInstallAgentAndFiles(t *testing.T) {
	root := t.TempDir()
	specDir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(specDir, "hello.sh"), []byte("echo hi\n"), 0o600))
	s, err := ParseSpec([]byte("name: demo\nbase: debian:12\nenv: {B: '2', A: '1'}\nfiles: [{src: hello.sh, dst: /usr/local/bin/hello, mode: '0755'}]\n"))
	must(t, err)
	img := &Image{Env: []string{"PATH=/usr/local/bin:/usr/bin"}, WorkDir: "/srv"}
	must(t, copyFiles(root, s, specDir))
	must(t, installAgent(root, []byte("agent-binary"), img, s))

	if fi, err := os.Stat(filepath.Join(root, "usr/local/bin/hello")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("copied file: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(root, agent.InstallPath)); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("agent: %v %v", fi, err)
	}
	gc, err := agent.LoadGuestConfig(filepath.Join(root, agent.GuestConfigPath))
	must(t, err)
	if strings.Join(gc.Env, " ") != "PATH=/usr/local/bin:/usr/bin A=1 B=2" || gc.Cwd != "/srv" {
		t.Errorf("guest config = %+v", gc)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/hostname")); string(b) != "demo\n" {
		t.Errorf("hostname = %q", b)
	}
}

func TestStore(t *testing.T) {
	data := t.TempDir()
	mk := func(id, name string, age time.Duration) {
		d := Dir(data, id)
		must(t, os.MkdirAll(d, 0o755))
		b, _ := json.Marshal(Template{ID: id, Name: name, CreatedAt: time.Now().Add(-age)})
		must(t, os.WriteFile(filepath.Join(d, MetaFile), b, 0o644))
	}
	mk("tpl-old", "python", 2*time.Hour)
	mk("tpl-new", "python", time.Hour)
	mk("tpl-node", "node", 0)
	must(t, os.MkdirAll(filepath.Join(Root(data), ".build-tpl-x"), 0o755)) // unfinished

	all, err := List(data)
	must(t, err)
	var ids []string
	for _, x := range all {
		ids = append(ids, x.ID)
	}
	if strings.Join(ids, ",") != "tpl-node,tpl-new,tpl-old" {
		t.Errorf("list = %v", ids)
	}
	if got, err := Get(data, "python"); err != nil || got.ID != "tpl-new" {
		t.Errorf("by name: %v %v", got, err)
	}
	if got, err := Get(data, "tpl-old"); err != nil || got.Name != "python" || got.Dir != Dir(data, "tpl-old") {
		t.Errorf("by id: %v %v", got, err)
	}
	if _, err := Get(data, "ruby"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := Remove(data, "tpl-new"); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(data, "python"); got == nil || got.ID != "tpl-old" {
		t.Errorf("after rm: %v", got)
	}
}

func TestFreeze(t *testing.T) {
	d := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(d, SnapshotDir), 0o755))
	for _, f := range []string{RootfsFile, MetaFile, filepath.Join(SnapshotDir, "mem"), filepath.Join(SnapshotDir, "vmstate"), filepath.Join(SnapshotDir, "snapshot.json")} {
		must(t, os.WriteFile(filepath.Join(d, f), []byte("x"), 0o644))
	}
	must(t, freeze(d))
	for f, want := range map[string]os.FileMode{
		RootfsFile: 0o444, filepath.Join(SnapshotDir, "mem"): 0o440, filepath.Join(SnapshotDir, "vmstate"): 0o440,
		filepath.Join(SnapshotDir, "snapshot.json"): 0o444, SnapshotDir: 0o555,
	} {
		fi, err := os.Stat(filepath.Join(d, f))
		must(t, err)
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", f, fi.Mode().Perm(), want)
		}
	}
	filepath.WalkDir(d, func(p string, e os.DirEntry, _ error) error { // let TempDir clean up
		if e.IsDir() {
			os.Chmod(p, 0o755)
		}
		return nil
	})
}

func TestShippedSpecsParse(t *testing.T) {
	files, _ := filepath.Glob("../../templates/*.yaml")
	files = append(files, "testdata/tiny.yaml")
	if len(files) < 3 {
		t.Fatalf("found %v", files)
	}
	for _, f := range files {
		s, err := LoadSpec(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if _, err := ID(s, filepath.Dir(f), []byte("v"), []byte("a")); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
