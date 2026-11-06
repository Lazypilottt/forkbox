package template

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agent"
	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/internal/vm"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

// Builder builds templates into <data>/templates/<id>.
type Builder struct {
	DataDir      string
	Manager      *vm.Manager
	Kernel       string // guest kernel (vmlinux) path
	KernelVer    string
	FCVersion    string
	VersionsYAML []byte
	AgentBinary  string
	GOARCH       string    // image platform, e.g. amd64
	Log          io.Writer // progress; nil discards

	// WarmUpTimeout bounds each warm-up command (default 10 minutes).
	WarmUpTimeout time.Duration
	// AgentTimeout bounds the wait for the agent after boot (default 60 s).
	AgentTimeout time.Duration

	pull func(ctx context.Context, ref, goarch string) (*Image, error)
}

// Build builds the spec at specPath unless a template with the same id
// already exists (then it returns that one, with Rebuilt false). force
// rebuilds anyway.
func (b *Builder) Build(ctx context.Context, specPath string, force bool) (t *Template, rebuilt bool, err error) {
	spec, err := LoadSpec(specPath)
	if err != nil {
		return nil, false, err
	}
	agentBin, err := os.ReadFile(b.AgentBinary)
	if err != nil {
		return nil, false, fmt.Errorf("agent binary: %w (build it with `make agent`)", err)
	}
	specDir := filepath.Dir(specPath)
	id, err := ID(spec, specDir, b.VersionsYAML, agentBin)
	if err != nil {
		return nil, false, err
	}
	logw := b.Log
	if logw == nil {
		logw = io.Discard
	}
	dir := Dir(b.DataDir, id)

	unlock, err := lock(b.DataDir, id)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	if !force {
		if t, err := Get(b.DataDir, id); err == nil {
			fmt.Fprintf(logw, "template %s (%s) is up to date\n", spec.Name, id)
			return t, false, nil
		}
	}

	staging := filepath.Join(Root(b.DataDir), ".build-"+id)
	if err := os.RemoveAll(staging); err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(staging) // a no-op after the rename below
	logFile, err := os.Create(filepath.Join(staging, BuildLogFile))
	if err != nil {
		return nil, false, err
	}
	defer logFile.Close()
	out := io.MultiWriter(logw, logFile)

	t = &Template{
		ID: id, Name: spec.Name, Spec: *spec, AgentSHA256: sum(agentBin),
		FirecrackerVersion: b.FCVersion, KernelVersion: b.KernelVer,
		Durations: map[string]float64{},
	}
	start := time.Now()
	step := func(name string, f func() error) error {
		fmt.Fprintf(out, "==> %s\n", name)
		t0 := time.Now()
		err := f()
		t.Durations[name] = time.Since(t0).Seconds()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}

	root := filepath.Join(staging, "root")
	pull := b.pull
	if pull == nil {
		pull = pullImage
	}
	var img *Image
	steps := []struct {
		name string
		f    func() error
	}{
		{StepPull, func() (err error) {
			img, err = pull(ctx, spec.Base, b.GOARCH)
			if err == nil {
				fmt.Fprintf(out, "    %s -> %s\n", spec.Base, img.Digest)
			}
			return err
		}},
		{StepExtract, func() error {
			st, err := extractImage(img, root, true)
			if err == nil {
				fmt.Fprintf(out, "    %d files (%.1f MiB), %d dirs, %d symlinks, %d hard links, %d skipped\n",
					st.Files, float64(st.Bytes)/(1<<20), st.Dirs, st.Symlinks, st.Hardlinks, len(st.Skipped))
			}
			return err
		}},
		{StepPackages, func() error { return installPackages(ctx, root, img, spec, out) }},
		{StepFiles, func() error { return copyFiles(root, spec, specDir) }},
		{StepAgent, func() error { return installAgent(root, agentBin, img, spec) }},
		{StepMkfs, func() error { return mkfs(ctx, root, filepath.Join(staging, RootfsFile), spec.DiskSizeMiB, out) }},
	}
	for _, s := range steps {
		if err := step(s.name, s.f); err != nil {
			return nil, false, fmt.Errorf("template %s: %w", spec.Name, err)
		}
	}
	if err := os.RemoveAll(root); err != nil {
		return nil, false, err
	}
	t.Image = *img
	if err := step(StepBoot, func() error { return b.bootAndSnapshot(ctx, t, staging, out) }); err != nil {
		return nil, false, fmt.Errorf("template %s: %w", spec.Name, err)
	}
	t.Durations[StepTotal] = time.Since(start).Seconds()
	t.CreatedAt = time.Now().UTC()
	if err := t.measure(staging); err != nil {
		return nil, false, err
	}
	if err := writeJSON(filepath.Join(staging, MetaFile), t); err != nil {
		return nil, false, err
	}
	if err := freeze(staging); err != nil {
		return nil, false, err
	}
	if err := os.RemoveAll(dir); err != nil { // --force over an old build
		return nil, false, err
	}
	if err := os.Rename(staging, dir); err != nil {
		return nil, false, err
	}
	t.Dir = dir
	fmt.Fprintf(out, "built template %s (%s) in %.1fs\n", spec.Name, id, t.Durations[StepTotal])
	return t, true, nil
}

// Build steps, as recorded in Template.Durations (seconds).
const (
	StepPull     = "pull"
	StepExtract  = "extract"
	StepPackages = "packages"
	StepFiles    = "files"
	StepAgent    = "agent"
	StepMkfs     = "mkfs"
	StepBoot     = "boot+warm_up+snapshot"
	StepTotal    = "total"
)

// chrootScript runs a command chrooted into $1 inside a private mount
// namespace (unshare --mount), so the /proc, /sys and /dev mounts vanish
// with it, even if the build is killed. Arguments are passed through "$@",
// never interpolated into the script.
const chrootScript = `set -eu
root=$1; shift
mount -t proc proc "$root/proc"
mount -t sysfs sysfs "$root/sys"
mount --rbind /dev "$root/dev"
exec chroot "$root" "$@"`

func chrootRun(ctx context.Context, root string, env []string, out io.Writer, argv ...string) error {
	for _, d := range []string{"proc", "sys", "dev"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return err
		}
	}
	args := append([]string{"--mount", "--propagation", "private", "--", "/bin/sh", "-c", chrootScript, "sh", root,
		"/usr/bin/env", "-i"}, env...)
	cmd := exec.CommandContext(ctx, "unshare", append(args, argv...)...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return nil
}

// imagePath is the image's PATH, or Debian's default.
func imagePath(img *Image) string {
	for _, e := range img.Env {
		if p, ok := strings.CutPrefix(e, "PATH="); ok {
			return p
		}
	}
	return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}

func installPackages(ctx context.Context, root string, img *Image, spec *Spec, out io.Writer) error {
	if len(spec.Apt) == 0 && len(spec.Pip) == 0 {
		fmt.Fprintln(out, "    nothing to install")
		return nil
	}
	// DNS for the package managers, and no service starts from postinst
	// scripts. Both are undone before the image is sealed.
	resolv := filepath.Join(root, "etc", "resolv.conf")
	saved, savedErr := os.ReadFile(resolv)
	_ = os.Remove(resolv) // may be a dangling symlink
	hostResolv, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return err
	}
	if err := os.WriteFile(resolv, hostResolv, 0o644); err != nil {
		return err
	}
	policy := filepath.Join(root, "usr", "sbin", "policy-rc.d")
	if err := os.WriteFile(policy, []byte("#!/bin/sh\nexit 101\n"), 0o755); err != nil {
		return err
	}
	defer func() {
		os.Remove(policy)
		os.Remove(resolv)
		if savedErr == nil {
			os.WriteFile(resolv, saved, 0o644)
		}
	}()
	env := []string{"PATH=" + imagePath(img), "HOME=/root", "LANG=C.UTF-8", "DEBIAN_FRONTEND=noninteractive"}
	if len(spec.Apt) > 0 {
		if err := chrootRun(ctx, root, env, out, "apt-get", "-q", "update"); err != nil {
			return err
		}
		install := append([]string{"apt-get", "-q", "install", "-y", "--no-install-recommends"}, spec.Apt...)
		if err := chrootRun(ctx, root, env, out, install...); err != nil {
			return err
		}
		if err := chrootRun(ctx, root, env, out, "apt-get", "clean"); err != nil {
			return err
		}
		lists, _ := filepath.Glob(filepath.Join(root, "var", "lib", "apt", "lists", "*"))
		for _, l := range lists {
			os.RemoveAll(l)
		}
	}
	if len(spec.Pip) > 0 {
		pip := append([]string{"python3", "-m", "pip", "install", "-q", "--no-cache-dir", "--root-user-action=ignore", "--disable-pip-version-check"}, spec.Pip...)
		if err := chrootRun(ctx, root, env, out, pip...); err != nil {
			return err
		}
	}
	return nil
}

func copyFiles(root string, spec *Spec, specDir string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range spec.Files {
		b, err := os.ReadFile(f.path(specDir))
		if err != nil {
			return err
		}
		m, _ := f.mode()
		dst := strings.TrimPrefix(filepath.Clean(f.Dst), "/")
		if err := r.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := r.WriteFile(dst, b, m); err != nil {
			return fmt.Errorf("%s: %w", f.Dst, err)
		}
		if err := r.Chmod(dst, m); err != nil {
			return err
		}
	}
	return nil
}

// installAgent puts the agent in the image as its init (kernel init=), with
// the Exec defaults: the image's environment, then the spec's env, and the
// image's working directory.
func installAgent(root string, bin []byte, img *Image, spec *Spec) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	p := strings.TrimPrefix(agent.InstallPath, "/")
	if err := r.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	_ = r.Remove(p)
	if err := r.WriteFile(p, bin, 0o755); err != nil {
		return err
	}
	if err := r.Chmod(p, 0o755); err != nil {
		return err
	}
	gc := agent.GuestConfig{Env: append(append([]string(nil), img.Env...), spec.EnvList()...), Cwd: img.WorkDir}
	b, err := json.MarshalIndent(gc, "", "  ")
	if err != nil {
		return err
	}
	cfg := strings.TrimPrefix(agent.GuestConfigPath, "/")
	if err := r.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		return err
	}
	if err := r.WriteFile(cfg, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return r.WriteFile("etc/hostname", []byte(spec.Name+"\n"), 0o644)
}

func mkfs(ctx context.Context, root, img string, sizeMiB int, out io.Writer) error {
	f, err := os.Create(img)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(sizeMiB) << 20); err != nil {
		f.Close()
		return err
	}
	f.Close()
	cmd := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F", "-L", "forkbox", "-d", root, img)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkfs.ext4 (is disk_size_mib %d large enough?): %w", sizeMiB, err)
	}
	return nil
}

// bootAndSnapshot boots the image with the agent as init, runs the warm-up
// commands through the agent, then pauses and takes the base snapshot.
func (b *Builder) bootAndSnapshot(ctx context.Context, t *Template, staging string, out io.Writer) error {
	cfg := vm.Config{
		ID:         "build-" + strings.TrimPrefix(t.ID, "tpl-")[:12],
		VCPUs:      t.Spec.VCPUs,
		MemMiB:     t.Spec.MemoryMiB,
		Kernel:     b.Kernel,
		KernelArgs: vm.DefaultKernelArgs + " init=" + agent.InstallPath,
		Rootfs:     vm.Drive{Path: filepath.Join(staging, RootfsFile)},
		Vsock:      &vm.Vsock{GuestCID: 3},
	}
	_ = b.Manager.Reap(cfg.ID) // a previous build killed half-way
	v, err := b.Manager.Boot(ctx, cfg)
	if err != nil {
		return err
	}
	defer v.Stop()
	c := agentclient.New(v.VsockPath(), agentwire.Port)
	at := b.AgentTimeout
	if at <= 0 {
		at = 60 * time.Second
	}
	actx, cancel := context.WithTimeout(ctx, at)
	p, err := c.WaitReady(actx)
	cancel()
	if err != nil {
		return fmt.Errorf("agent did not answer within %s: %w\nconsole:\n%s", at, err, tail(v.ConsolePath()))
	}
	fmt.Fprintf(out, "    agent %s up\n", p.AgentVersion)
	wt := b.WarmUpTimeout
	if wt <= 0 {
		wt = 10 * time.Minute
	}
	t0 := time.Now()
	for _, cmd := range t.Spec.WarmUp {
		fmt.Fprintf(out, "    $ %s\n", cmd)
		exit, err := c.Exec(ctx, agentclient.ExecRequest{Args: []string{"/bin/sh", "-c", cmd}, Timeout: wt},
			func(_ agentclient.Stream, d []byte) { out.Write(d) })
		if err != nil {
			return fmt.Errorf("warm_up %q: %w", cmd, err)
		}
		if exit.Code != 0 || exit.TimedOut {
			return fmt.Errorf("warm_up %q: exit %d signal %q timed out %v", cmd, exit.Code, exit.Signal, exit.TimedOut)
		}
	}
	t.Durations["warm_up"] = time.Since(t0).Seconds()
	if err := v.Pause(ctx); err != nil {
		return err
	}
	snap, err := v.SnapshotFull(ctx, filepath.Join(staging, SnapshotDir))
	if err != nil {
		return err
	}
	for _, tm := range v.Timings() {
		if tm.Step == vm.StepBoot || tm.Step == vm.StepSnapshotFull {
			t.Durations["vm-"+tm.Step] = tm.Duration.Seconds()
		}
	}
	t.VCPUs, t.MemMiB = snap.VCPUs, snap.MemMiB
	return v.Stop()
}

func tail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(b) > 4096 {
		b = b[len(b)-4096:]
	}
	return string(b)
}

// lock serialises builds of one id across processes.
func lock(dataDir, id string) (func(), error) {
	if err := os.MkdirAll(Root(dataDir), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(Root(dataDir), ".lock-"+id), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		os.Remove(f.Name())
	}, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// freeze makes a finished template read-only: files 0444 (the memory file
// stays root:<jail gid> 0440 so jailed VMs can map it), directories 0555.
func freeze(dir string) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.Chmod(p, 0o555)
		case filepath.Base(filepath.Dir(p)) == SnapshotDir && (d.Name() == vm.SnapshotMemFile || d.Name() == vm.SnapshotStateFile):
			return os.Chmod(p, 0o440)
		default:
			if err := os.Lchown(p, 0, 0); err != nil && !errors.Is(err, os.ErrPermission) {
				return err
			}
			return os.Chmod(p, 0o444)
		}
	})
}
