package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Lazypilottt/forkbox/internal/datadir"
	"github.com/Lazypilottt/forkbox/internal/versions"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type fakeRelease struct {
	archive  []byte
	kernel   []byte
	bins     map[string][]byte
	requests atomic.Int32
	srv      *httptest.Server
}

// newFakeRelease serves a Firecracker-shaped release archive and a kernel.
func newFakeRelease(t *testing.T, extra map[string][]byte) *fakeRelease {
	t.Helper()
	r := &fakeRelease{
		kernel: []byte("not really a kernel"),
		bins: map[string][]byte{
			"firecracker":     []byte("#!fake firecracker"),
			"jailer":          []byte("#!fake jailer"),
			"snapshot-editor": []byte("#!fake snapshot-editor"),
		},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	add("release-v9.9.9-x86_64/LICENSE", []byte("license"))
	for name, body := range r.bins {
		add("release-v9.9.9-x86_64/"+name+"-v9.9.9-x86_64", body)
		add("release-v9.9.9-x86_64/"+name+"-v9.9.9-x86_64.debug", []byte("debug symbols"))
	}
	for name, body := range extra {
		add(name, body)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	r.archive = buf.Bytes()
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.requests.Add(1)
		switch req.URL.Path {
		case "/release/firecracker-v9.9.9-x86_64.tgz":
			w.Write(r.archive)
		case "/kernel/vmlinux-6.18.1":
			w.Write(r.kernel)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRelease) versions() *versions.File {
	bins := map[string]string{}
	for name, body := range r.bins {
		bins[name] = sum(body)
	}
	return &versions.File{
		Schema: 1,
		Firecracker: versions.Firecracker{
			Version:    "v9.9.9",
			ReleaseURL: r.srv.URL + "/release",
			Artifacts: map[string]versions.FirecrackerArtifact{"x86_64": {
				Archive: "firecracker-v9.9.9-x86_64.tgz", SHA256: sum(r.archive), Binaries: bins,
			}},
		},
		GuestKernel: versions.Download{Version: "6.18.1", Artifacts: map[string]versions.Artifact{
			"x86_64": {URL: r.srv.URL + "/kernel/vmlinux-6.18.1", SHA256: sum(r.kernel)},
		}},
	}
}

func opts(dir string, v *versions.File) Options {
	return Options{DataDir: dir, Versions: v, Arch: "x86_64"}
}

func TestInstallsAndVerifies(t *testing.T) {
	rel := newFakeRelease(t, nil)
	dir := t.TempDir()
	inst, err := Run(context.Background(), opts(dir, rel.versions()))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range rel.bins {
		path := datadir.Bin(dir, name)
		if inst.Binaries[name] != path {
			t.Errorf("Installed[%s] = %s, want %s", name, inst.Binaries[name], path)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, body) {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o755 {
			t.Errorf("%s mode %v, want 0755", name, fi.Mode().Perm())
		}
	}
	if got, _ := os.ReadFile(datadir.Kernel(dir, "6.18.1")); !bytes.Equal(got, rel.kernel) {
		t.Errorf("kernel not installed: %q", got)
	}
	if _, err := os.Stat(datadir.Bin(dir, "firecracker.debug")); err == nil {
		t.Error("debug symbols should not be installed")
	}
	assertNoPartFiles(t, dir)
}

func TestSecondRunIsANoOp(t *testing.T) {
	rel := newFakeRelease(t, nil)
	dir := t.TempDir()
	if _, err := Run(context.Background(), opts(dir, rel.versions())); err != nil {
		t.Fatal(err)
	}
	before := rel.requests.Load()
	var log bytes.Buffer
	o := opts(dir, rel.versions())
	o.Log = &log
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if n := rel.requests.Load() - before; n != 0 {
		t.Errorf("second run made %d HTTP requests, want 0", n)
	}
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if !strings.HasPrefix(line, "ok ") {
			t.Errorf("second run did work: %q", line)
		}
	}
}

func TestReplacesTamperedBinaryFromCachedArchive(t *testing.T) {
	rel := newFakeRelease(t, nil)
	dir := t.TempDir()
	if _, err := Run(context.Background(), opts(dir, rel.versions())); err != nil {
		t.Fatal(err)
	}
	jailer := datadir.Bin(dir, "jailer")
	if err := os.WriteFile(jailer, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := rel.requests.Load()
	if _, err := Run(context.Background(), opts(dir, rel.versions())); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(jailer); !bytes.Equal(got, rel.bins["jailer"]) {
		t.Errorf("jailer not restored: %q", got)
	}
	if n := rel.requests.Load() - before; n != 0 {
		t.Errorf("re-install used the network (%d requests); the archive is cached", n)
	}
}

func TestForceRedownloads(t *testing.T) {
	rel := newFakeRelease(t, nil)
	dir := t.TempDir()
	if _, err := Run(context.Background(), opts(dir, rel.versions())); err != nil {
		t.Fatal(err)
	}
	before := rel.requests.Load()
	o := opts(dir, rel.versions())
	o.Force = true
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if n := rel.requests.Load() - before; n != 2 {
		t.Errorf("--force made %d requests, want 2 (archive and kernel)", n)
	}
}

func TestArchiveChecksumMismatch(t *testing.T) {
	rel := newFakeRelease(t, nil)
	v := rel.versions()
	a := v.Firecracker.Artifacts["x86_64"]
	a.SHA256 = strings.Repeat("0", 64)
	v.Firecracker.Artifacts["x86_64"] = a
	dir := t.TempDir()
	_, err := Run(context.Background(), opts(dir, v))
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if _, err := os.Stat(filepath.Join(datadir.Downloads(dir), a.Archive)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a mismatched download must not be left in place")
	}
	if _, err := os.Stat(datadir.Bin(dir, "firecracker")); !errors.Is(err, os.ErrNotExist) {
		t.Error("nothing should be installed after a mismatch")
	}
	assertNoPartFiles(t, dir)
}

func TestBinaryChecksumMismatch(t *testing.T) {
	rel := newFakeRelease(t, nil)
	v := rel.versions()
	a := v.Firecracker.Artifacts["x86_64"]
	a.Binaries["jailer"] = strings.Repeat("1", 64)
	dir := t.TempDir()
	_, err := Run(context.Background(), opts(dir, v))
	if !errors.Is(err, ErrChecksum) || !strings.Contains(err.Error(), "jailer-v9.9.9-x86_64") {
		t.Fatalf("err = %v, want ErrChecksum naming the jailer", err)
	}
	if _, err := os.Stat(datadir.Bin(dir, "jailer")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a mismatched jailer must not be installed")
	}
	assertNoPartFiles(t, dir)
}

func TestKernelChecksumMismatch(t *testing.T) {
	rel := newFakeRelease(t, nil)
	v := rel.versions()
	k := v.GuestKernel.Artifacts["x86_64"]
	k.SHA256 = strings.Repeat("2", 64)
	v.GuestKernel.Artifacts["x86_64"] = k
	dir := t.TempDir()
	if _, err := Run(context.Background(), opts(dir, v)); !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if _, err := os.Stat(datadir.Kernel(dir, "6.18.1")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a mismatched kernel must not be installed")
	}
}

func TestMissingMember(t *testing.T) {
	rel := newFakeRelease(t, nil)
	v := rel.versions()
	v.Firecracker.Artifacts["x86_64"].Binaries["rebase-snap"] = strings.Repeat("3", 64)
	_, err := Run(context.Background(), opts(t.TempDir(), v))
	if err == nil || !strings.Contains(err.Error(), "rebase-snap-v9.9.9-x86_64") {
		t.Fatalf("err = %v, want one naming the missing member", err)
	}
}

func TestIgnoresHostileMembers(t *testing.T) {
	// Only exactly-named members are extracted, so path tricks in the archive
	// cannot write outside the data directory.
	rel := newFakeRelease(t, map[string][]byte{"../../escape": []byte("x"), "/etc/evil": []byte("x")})
	dir := filepath.Join(t.TempDir(), "data")
	if _, err := Run(context.Background(), opts(dir, rel.versions())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "..", "escape")); err == nil {
		t.Error("hostile member was extracted")
	}
}

func TestHTTPError(t *testing.T) {
	rel := newFakeRelease(t, nil)
	v := rel.versions()
	k := v.GuestKernel.Artifacts["x86_64"]
	k.URL = rel.srv.URL + "/kernel/missing"
	v.GuestKernel.Artifacts["x86_64"] = k
	_, err := Run(context.Background(), opts(t.TempDir(), v))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want an HTTP 404", err)
	}
}

func TestUnpinnedArch(t *testing.T) {
	rel := newFakeRelease(t, nil)
	o := opts(t.TempDir(), rel.versions())
	o.Arch = "aarch64"
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "aarch64") {
		t.Fatalf("err = %v, want one naming aarch64", err)
	}
	if n := rel.requests.Load(); n != 0 {
		t.Errorf("made %d requests for an unpinned arch", n)
	}
}

func assertNoPartFiles(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), ".part-") {
			t.Errorf("leftover temporary file %s", path)
		}
		return nil
	})
}

func TestFetchVerified(t *testing.T) {
	body := []byte("pinned rootfs")
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Write(body)
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "rootfs")

	if err := FetchVerified(context.Background(), nil, nil, srv.URL, dst, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != string(body) {
		t.Fatalf("dst = %q", got)
	}
	if err := FetchVerified(context.Background(), nil, nil, srv.URL, dst, want); err != nil || hits != 1 {
		t.Fatalf("second fetch: err=%v hits=%d, want no download", err, hits)
	}
	other := filepath.Join(t.TempDir(), "x")
	if err := FetchVerified(context.Background(), nil, nil, srv.URL, other, strings.Repeat("0", 64)); !errors.Is(err, ErrChecksum) {
		t.Fatalf("bad checksum: got %v", err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("unverified file was installed")
	}
}
