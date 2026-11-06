package template

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Why go-containerregistry: it pulls and flattens OCI and Docker images in
// pure Go (layer digests verified, whiteouts applied), so a build needs no
// container daemon, skopeo or umoci on the host.

// Image is a pulled base image.
type Image struct {
	Ref     string   `json:"ref"`
	Digest  string   `json:"digest"` // manifest digest actually used
	Env     []string `json:"env"`    // from the image config
	WorkDir string   `json:"workdir"`
	img     v1.Image
}

// pullImage resolves ref for linux/<arch> and reads its config. Layers are
// fetched lazily while extracting.
func pullImage(ctx context.Context, ref, goarch string) (*Image, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("image %q: %w", ref, err)
	}
	img, err := remote.Image(r,
		remote.WithContext(ctx),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: goarch}))
	if err != nil {
		return nil, fmt.Errorf("pulling %s: %w", ref, err)
	}
	return imageFrom(ref, img)
}

func imageFrom(ref string, img v1.Image) (*Image, error) {
	d, err := img.Digest()
	if err != nil {
		return nil, err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("image config: %w", err)
	}
	return &Image{Ref: ref, Digest: d.String(), Env: cf.Config.Env, WorkDir: cf.Config.WorkingDir, img: img}, nil
}

// extractStats counts what extraction did.
type extractStats struct {
	Files, Dirs, Symlinks, Hardlinks int
	Skipped                          []string // device nodes and fifos
	Bytes                            int64
}

// extractImage writes the image's flattened filesystem into dst. Every
// operation goes through an os.Root, so neither ".." names nor symlinks in
// the image can place a file outside dst. Ownership is applied only when
// chown is set (it needs root).
func extractImage(img *Image, dst string, chown bool) (*extractStats, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rc := mutate.Extract(img.img)
	defer rc.Close()
	return extractTar(rc, root, chown)
}

type deferredLink struct {
	oldname, newname string
	hdr              *tar.Header
}

func extractTar(r io.Reader, root *os.Root, chown bool) (*extractStats, error) {
	st := &extractStats{}
	tr := tar.NewReader(r)
	var links []deferredLink
	dirs := map[string]*tar.Header{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return st, fmt.Errorf("reading image: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "/"))
		if name == "." {
			continue
		}
		if name == ".." || strings.HasPrefix(name, "../") {
			return st, fmt.Errorf("image entry %q escapes the root", hdr.Name)
		}
		// Upper layers come first, so a file can precede its parent's entry.
		if dir := path.Dir(name); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return st, fmt.Errorf("%s: %w", dir, err)
			}
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return st, fmt.Errorf("%s: %w", name, err)
			}
			dirs[name] = hdr
			st.Dirs++
		case tar.TypeReg:
			_ = root.Remove(name)
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return st, fmt.Errorf("%s: %w", name, err)
			}
			n, err := io.Copy(f, tr)
			if err == nil {
				err = setXattrs(f, hdr)
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return st, fmt.Errorf("%s: %w", name, err)
			}
			if err := applyMeta(root, name, hdr, chown); err != nil {
				return st, err
			}
			st.Files++
			st.Bytes += n
		case tar.TypeSymlink:
			_ = root.Remove(name)
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return st, fmt.Errorf("%s: %w", name, err)
			}
			if chown {
				if err := root.Lchown(name, hdr.Uid, hdr.Gid); err != nil {
					return st, fmt.Errorf("%s: %w", name, err)
				}
			}
			st.Symlinks++
		case tar.TypeLink:
			// The target may come later in the stream (a lower layer).
			links = append(links, deferredLink{path.Clean(strings.TrimPrefix(hdr.Linkname, "/")), name, hdr})
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// Images ship no usable device nodes; the guest's /dev is devtmpfs.
			st.Skipped = append(st.Skipped, name)
		default:
			st.Skipped = append(st.Skipped, name)
		}
	}
	for _, l := range links {
		_ = root.Remove(l.newname)
		if err := root.Link(l.oldname, l.newname); err != nil {
			return st, fmt.Errorf("hard link %s -> %s: %w", l.newname, l.oldname, err)
		}
		st.Hardlinks++
	}
	// Directories last, deepest first: creating entries inside them would
	// otherwise reset mtimes, and a read-only directory would block them.
	names := make([]string, 0, len(dirs))
	for n := range dirs {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return strings.Count(names[i], "/") > strings.Count(names[j], "/") })
	for _, n := range names {
		if err := applyMeta(root, n, dirs[n], chown); err != nil {
			return st, err
		}
	}
	return st, nil
}

// applyMeta sets owner, then mode (chown clears setuid), then times.
func applyMeta(root *os.Root, name string, hdr *tar.Header, chown bool) error {
	if chown {
		if err := root.Lchown(name, hdr.Uid, hdr.Gid); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := root.Chmod(name, hdr.FileInfo().Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky)); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	mt := hdr.ModTime
	if mt.IsZero() {
		mt = time.Unix(0, 0)
	}
	if err := root.Chtimes(name, mt, mt); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
