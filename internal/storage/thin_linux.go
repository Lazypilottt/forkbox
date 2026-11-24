package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Thin is the dmthin backend: one device-mapper thin pool (PoolName) on
// two sparse files in Dir attached to loop devices. A template's rootfs is
// imported into a thin volume once; every sandbox disk is a thin snapshot,
// shared with its origin until written. A disk in the data directory is a
// block device node for its volume, with a record next to it
// (<disk>.dmthin) saying which volume. Every volume id is also recorded in
// Dir/ids, so Reap can free volumes nothing records any more.
//
// Block zeroing stays on: a block a sandbox writes for the first time
// never shows another sandbox's old data.
type Thin struct {
	DataDir  string
	Dir      string // <data>/dmthin
	PoolName string // device-mapper name, fbx-pool
	// DataSize is the pool's data file (sparse) in bytes.
	DataSize int64
	// Run runs a command and returns its output; tests replace it.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)

	mu sync.Mutex
}

// DefaultThinSize is the default pool size (a sparse file: it takes only
// what sandboxes write).
const DefaultThinSize = 100 << 30

// thinBlockSectors is the pool's block size: 64 KiB.
const thinBlockSectors = 128

// NewThin returns the dmthin backend for dataDir.
func NewThin(dataDir string, size int64) *Thin {
	if size <= 0 {
		size = DefaultThinSize
	}
	return &Thin{DataDir: dataDir, Dir: filepath.Join(dataDir, "dmthin"), PoolName: "fbx-pool", DataSize: size, Run: runCmd}
}

func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

func (*Thin) Name() string { return BackendDMThin }

func (t *Thin) dm(ctx context.Context, args ...string) error {
	_, err := t.Run(ctx, "dmsetup", args...)
	return err
}

func (t *Thin) message(ctx context.Context, msg string) error {
	return t.dm(ctx, "message", t.PoolName, "0", msg)
}

// volName is the device-mapper name of a volume: <pool>-<id>.
func (t *Thin) volName(id int) string { return t.PoolName + "-" + strconv.Itoa(id) }

func devPath(name string) string { return "/dev/mapper/" + name }

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Ensure creates the pool, or reattaches it after a reboot.
func (t *Thin) Ensure(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if exists(devPath(t.PoolName)) {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(t.Dir, "ids"), 0o700); err != nil {
		return err
	}
	data := filepath.Join(t.Dir, "data.img")
	meta := filepath.Join(t.Dir, "meta.img")
	metaSize := min(max(t.DataSize/512, 64<<20), 16<<30)
	for _, f := range []struct {
		path string
		size int64
	}{{data, t.DataSize}, {meta, metaSize}} {
		if exists(f.path) {
			continue
		}
		// New and sparse, so the metadata starts zeroed (a new pool).
		file, err := os.OpenFile(f.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		err = file.Truncate(f.size)
		file.Close()
		if err != nil {
			return err
		}
	}
	st, err := os.Stat(data)
	if err != nil {
		return err
	}
	var loops []string
	for _, f := range []string{meta, data} {
		dev, err := t.loopFor(ctx, f)
		if err != nil {
			return err
		}
		loops = append(loops, dev)
	}
	sectors := st.Size() / 512 / thinBlockSectors * thinBlockSectors
	table := fmt.Sprintf("0 %d thin-pool %s %s %d 32768", sectors, loops[0], loops[1], thinBlockSectors)
	if err := t.dm(ctx, "create", t.PoolName, "--table", table); err != nil {
		return fmt.Errorf("creating the thin pool: %w", err)
	}
	return nil
}

// loopFor returns the loop device attached to file, attaching one if none is.
func (t *Thin) loopFor(ctx context.Context, file string) (string, error) {
	out, err := t.Run(ctx, "losetup", "-j", file)
	if err != nil {
		return "", err
	}
	if dev, _, ok := strings.Cut(strings.TrimSpace(string(out)), ":"); ok && strings.HasPrefix(dev, "/dev/loop") {
		return dev, nil
	}
	out, err = t.Run(ctx, "losetup", "-f", "--show", file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// thinRecord is a disk's <disk>.dmthin file.
type thinRecord struct {
	ID      int   `json:"id"`
	Sectors int64 `json:"sectors"`
}

func readRecord(path string) (*thinRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r thinRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

func writeRecord(path string, r thinRecord) error {
	b, _ := json.Marshal(r)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// allocID reserves a new volume id (Dir/ids/<id>); t.mu is held.
func (t *Thin) allocID() (int, error) {
	counter := filepath.Join(t.Dir, "next-id")
	n := 1
	if b, err := os.ReadFile(counter); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	for exists(filepath.Join(t.Dir, "ids", strconv.Itoa(n))) {
		n++
	}
	if n >= 1<<24 {
		return 0, errors.New("dmthin: out of volume ids")
	}
	if err := os.WriteFile(filepath.Join(t.Dir, "ids", strconv.Itoa(n)), nil, 0o600); err != nil {
		return 0, err
	}
	return n, os.WriteFile(counter, []byte(strconv.Itoa(n+1)), 0o600)
}

func (t *Thin) activate(ctx context.Context, id int, sectors int64) error {
	if exists(devPath(t.volName(id))) {
		return nil
	}
	return t.dm(ctx, "create", t.volName(id), "--table", fmt.Sprintf("0 %d thin %s %d", sectors, devPath(t.PoolName), id))
}

func (t *Thin) deactivate(ctx context.Context, id int) error {
	if !exists(devPath(t.volName(id))) {
		return nil
	}
	var err error
	for range 20 { // the VM may still be letting go of it
		if err = t.dm(ctx, "remove", t.volName(id)); err == nil || !exists(devPath(t.volName(id))) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// drop deactivates and deletes a volume and its id; t.mu is held.
func (t *Thin) drop(ctx context.Context, id int) error {
	if err := t.deactivate(ctx, id); err != nil {
		return err
	}
	if err := t.message(ctx, fmt.Sprintf("delete %d", id)); err != nil && !strings.Contains(err.Error(), "No such") {
		return err
	}
	return os.Remove(filepath.Join(t.Dir, "ids", strconv.Itoa(id)))
}

// importKey names the import of an image file: its path, inode and mtime.
func importKey(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d:%d", path, st.Ino, st.Mtim.Sec, st.Mtim.Nsec)))
	return hex.EncodeToString(h[:12]), nil
}

// origin returns the volume behind src: its own record, or an import of the
// image file (made once, kept in Dir/imports); t.mu is held.
func (t *Thin) origin(ctx context.Context, src string) (*thinRecord, error) {
	if r, err := readRecord(src + thinMetaSuffix); err == nil {
		return r, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key, err := importKey(src)
	if err != nil {
		return nil, err
	}
	rec := filepath.Join(t.Dir, "imports", key+".json")
	if r, err := readRecord(rec); err == nil {
		return r, nil
	}
	if err := os.MkdirAll(filepath.Dir(rec), 0o700); err != nil {
		return nil, err
	}
	st, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	if st.Size()%512 != 0 {
		return nil, fmt.Errorf("dmthin: %s is not a whole number of sectors", src)
	}
	id, err := t.allocID()
	if err != nil {
		return nil, err
	}
	r := thinRecord{ID: id, Sectors: st.Size() / 512}
	fail := func(err error) (*thinRecord, error) {
		_ = t.drop(context.WithoutCancel(ctx), id)
		return nil, fmt.Errorf("dmthin: importing %s: %w", src, err)
	}
	if err := t.message(ctx, fmt.Sprintf("create_thin %d", id)); err != nil {
		return fail(err)
	}
	if err := t.activate(ctx, id, r.Sectors); err != nil {
		return fail(err)
	}
	if err := copySparse(src, devPath(t.volName(id))); err != nil {
		return fail(err)
	}
	if err := t.deactivate(ctx, id); err != nil {
		return fail(err)
	}
	// Records the source path too, so Reap can drop imports of removed images.
	b, _ := json.Marshal(struct {
		thinRecord
		Source string `json:"source"`
	}{r, src})
	if err := os.WriteFile(rec, b, 0o600); err != nil {
		return fail(err)
	}
	return &r, nil
}

// Clone makes dst a thin snapshot of src.
func (t *Thin) Clone(ctx context.Context, src, dst string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if exists(dst) {
		return fmt.Errorf("dmthin: %s exists", dst)
	}
	o, err := t.origin(ctx, src)
	if err != nil {
		return err
	}
	id, err := t.allocID()
	if err != nil {
		return err
	}
	fail := func(err error) error {
		os.Remove(dst)
		os.Remove(dst + thinMetaSuffix)
		_ = t.drop(context.WithoutCancel(ctx), id)
		return fmt.Errorf("dmthin: %s -> %s: %w", src, dst, err)
	}
	// An active origin is suspended while the snapshot is taken, so its
	// in-flight writes are all in or all out.
	active := exists(devPath(t.volName(o.ID)))
	if active {
		if err := t.dm(ctx, "suspend", t.volName(o.ID)); err != nil {
			return fail(err)
		}
	}
	err = t.message(ctx, fmt.Sprintf("create_snap %d %d", id, o.ID))
	if active {
		if rerr := t.dm(context.WithoutCancel(ctx), "resume", t.volName(o.ID)); rerr != nil && err == nil {
			err = rerr
		}
	}
	if err != nil {
		return fail(err)
	}
	if err := t.activate(ctx, id, o.Sectors); err != nil {
		return fail(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(devPath(t.volName(id)), &st); err != nil {
		return fail(err)
	}
	if err := writeRecord(dst+thinMetaSuffix, thinRecord{ID: id, Sectors: o.Sectors}); err != nil {
		return fail(err)
	}
	if err := unix.Mknod(dst, unix.S_IFBLK|0o600, int(st.Rdev)); err != nil {
		return fail(err)
	}
	return nil
}

// Remove frees the volume behind path and removes the node.
func (t *Thin) Remove(ctx context.Context, path string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.removeLocked(ctx, path)
}

func (t *Thin) removeLocked(ctx context.Context, path string) error {
	r, err := readRecord(path + thinMetaSuffix)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := t.drop(ctx, r.ID); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	os.Remove(path)
	return os.Remove(path + thinMetaSuffix)
}

// RemoveAll frees every disk under dir, then removes dir.
func (t *Thin) RemoveAll(ctx context.Context, dir string) error {
	t.mu.Lock()
	var errs []error
	for _, m := range diskMetas(dir) {
		errs = append(errs, t.removeLocked(ctx, strings.TrimSuffix(m, thinMetaSuffix)))
	}
	t.mu.Unlock()
	errs = append(errs, os.RemoveAll(dir))
	return errors.Join(errs...)
}

// Reap frees volumes no disk record or live import refers to (left by a
// crash between allocating and recording), and imports whose image is gone.
func (t *Thin) Reap(ctx context.Context) ([]string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	used := map[int]bool{}
	for _, m := range diskMetas(t.DataDir) {
		if r, err := readRecord(m); err == nil {
			used[r.ID] = true
		}
	}
	var done []string
	var errs []error
	imports, _ := filepath.Glob(filepath.Join(t.Dir, "imports", "*.json"))
	for _, p := range imports {
		var r struct {
			thinRecord
			Source string `json:"source"`
		}
		b, err := os.ReadFile(p)
		if err != nil || json.Unmarshal(b, &r) != nil {
			continue
		}
		key, kerr := importKey(r.Source)
		if kerr == nil && filepath.Base(p) == key+".json" {
			used[r.ID] = true
			continue
		}
		// The image was removed or rebuilt. Snapshots of it stay valid:
		// thin snapshots do not depend on their origin's id.
		if err := t.drop(ctx, r.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		os.Remove(p)
		done = append(done, "import "+r.Source)
	}
	ids, _ := os.ReadDir(filepath.Join(t.Dir, "ids"))
	for _, e := range ids {
		id, err := strconv.Atoi(e.Name())
		if err != nil || used[id] {
			continue
		}
		if err := t.drop(ctx, id); err != nil {
			errs = append(errs, err)
			continue
		}
		done = append(done, t.volName(id))
	}
	return done, errors.Join(errs...)
}

// thinMetaSuffix marks the file that records which thin volume backs a disk.
const thinMetaSuffix = ".dmthin"

// diskMetas lists the thin-volume records under dir.
func diskMetas(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, thinMetaSuffix) {
			out = append(out, p)
		}
		return nil
	})
	return out
}
