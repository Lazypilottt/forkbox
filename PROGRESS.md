# Forkbox progress

| Task | Status |
| --- | --- |
| 1. Scaffold, `forkbox doctor` and `forkbox setup` | **Built; acceptance pending on a KVM host.** All checks that can run on macOS pass. |
| 2–17 | Not started. They need a Linux KVM host. |

**Resume point:** Task 1 acceptance on the KVM host (see "How to try it" below), then Task 2.

---

## Task 1. Scaffold, `forkbox doctor` and `forkbox setup` (2026-11-06)

### What was built

- **Layout:** `cmd/{forkboxd,forkbox,forkbox-agent}`,
  `internal/{vm,storage,pagesrv,netns,template,api,store}`, `proto/`, `sdk/python/`,
  `mcp/`, `bench/`, `templates/`, `docs/`. The daemon and agent mains are stubs
  until their tasks. Apache-2.0 `LICENSE` and a README skeleton.
- **`versions.yaml`:** every pin carries a SHA-256. The file is embedded in the
  binaries; `--versions FILE` overrides it, and parsing is strict.
  - Firecracker and jailer v1.17.0. Pins the archive checksum and the checksums
    of `firecracker`, `jailer` and `snapshot-editor`.
  - Guest kernel 6.18.56 and the Ubuntu 24.04 rootfs, both from Firecracker CI
    build `20261105-6ea6634994fa-0`.
  - x86_64 only.
- **`forkbox doctor`:** 11 checks. Each prints one PASS or FAIL line, every
  failure has a `fix:` line, and the exit status is 1 if any check fails.
  - `platform`: Linux, and this architecture is pinned.
  - `kvm`:
    - `/dev/kvm` is a char device the user can read and write;
    - the fix names `kvm_intel` or `kvm_amd`;
    - detects disabled VT-x/AMD-V;
    - inside Docker or Podman, suggests `--device /dev/kvm`.
  - `kernel`: 5.10 or newer.
  - `cgroup-v2`: cgroup2 mounted, with the cpu and memory controllers.
  - `userfaultfd`: `/dev/userfaultfd` on kernel 6.1 or newer, else
    `vm.unprivileged_userfaultfd=1`.
  - `data-dir-fs`: XFS or btrfs.
  - `data-dir-reflink`: a real FICLONE copy, with its bytes checked.
  - `firecracker`, `jailer`: the checksum matches the pin, and `--version`
    reports `v1.17.0`.
  - `nftables`, `iproute2`: present and from the right package.
- **`forkbox setup`:**
  - downloads the release archive and the kernel;
  - writes each file to a temporary path and renames it into place only after
    its checksum matches;
  - extracts only the exactly named archive members;
  - keeps the archive, so repairs need no network;
  - is a no-op when everything already verifies; `--force` re-downloads.
- **`internal/storage`:** `FSType`, `Reflink` (never falls back to a byte copy)
  and `ProbeReflink`.
- **Tests:** integration tests use the build tag `integration`, and
  `testutil.RequireKVM` skips them cleanly where `/dev/kvm` is missing.
- **CI:**
  - unit job: gofmt, vet, staticcheck 2026.2.2 and `go test -race`;
  - pytest step: switches on once `sdk/python/pyproject.toml` exists;
  - integration job: runs only on `[self-hosted, linux, kvm]`, never for pull
    requests from forks, one job at a time.

### Notes from the Firecracker v1.17.0 docs

- The 6.1 guest kernel's minimum support ended 2026-09-02, so the pinned guest
  kernel is 6.18.
- CI artifacts now live under dated `firecracker-ci/YYYYMMDD-<sha>-0/` prefixes.
- The jailer's `--cgroup-version` defaults to 1, so Task 2 must pass
  `--cgroup-version 2`.
- On kernel 6.1 or newer, the jailer exposes `/dev/userfaultfd` in the jail.

### How to try it on the KVM host

```sh
go build -o bin/forkbox ./cmd/forkbox
sudo bin/forkbox setup          # about 35 MB, 5 checksums verified
sudo bin/forkbox doctor         # expect 11 PASS
make check && make integration
CGO_ENABLED=0 go build -o bin/forkbox-static ./cmd/forkbox
docker run --rm -v "$PWD/bin/forkbox-static:/forkbox:ro" ubuntu:24.04 /forkbox doctor
#   expect: FAIL kvm "/dev/kvm does not exist inside this container"
```

### Test results (macOS arm64, Go 1.27.0)

- gofmt, `go vet` and staticcheck pass, also with `GOOS=linux GOARCH=amd64`.
- `go test ./...`: 98 passed, 0 failed (counting subtests).
- `forkbox doctor` on macOS: 11 FAIL with the platform fix shown, exit 1.
- **Not run yet:** the Linux-only storage tests, the KVM integration test and
  the container check.

### Limitations

- x86_64 only.
- The kernel and rootfs URLs depend on Firecracker CI's dated S3 prefix.
- Fix commands assume Ubuntu or Debian.
- Reference invocation is `sudo forkbox doctor`.

### Open questions

- On kernels older than 6.1, requiring `vm.unprivileged_userfaultfd=1` is an
  inference; the docs only say 5.10 uses the syscall. Verify in Task 12.
  Ubuntu 24.04 (kernel 6.8) is unaffected.

---

## Blocked

- Everything from Task 1's acceptance onward needs a Linux x86_64 KVM host.
  Apple Silicon M1 Macs have no nested virtualisation.

## TODO (manual)

1. **KVM host:**
   - Ubuntu 24.04, at least 8 vCPUs and 16 GB RAM;
   - XFS (`mkfs.xfs -m reflink=1`) or btrfs mounted at `/var/lib/forkbox`.

   Clone the repo there and run the steps above.
2. **Register the host as the `kvm` runner.**
   1. On GitHub: repo → Settings → Actions → Runners → New self-hosted runner →
      Linux x64. Run the download commands it shows, as a dedicated user
      `runner`.
   2. Configure and install the service:
      ```sh
      ./config.sh --url https://github.com/Lazypilottt/forkbox --token <TOKEN> \
          --name forkbox-kvm --labels kvm --unattended
      sudo ./svc.sh install runner && sudo ./svc.sh start
      ```
   3. Give `runner` passwordless sudo; CI uses `sudo -n`:
      ```sh
      echo 'runner ALL=(root) NOPASSWD: ALL' | sudo tee /etc/sudoers.d/forkbox-runner
      sudo chmod 0440 /etc/sudoers.d/forkbox-runner && sudo visudo -c
      ```
   4. If the repo is ever made public, enable "Require approval for all
      external contributors".

## Ideas

- `forkbox doctor --json`.
- Mirror the pinned kernel and rootfs to a Forkbox release.
- Pin aarch64 artifacts.
- Doctor checks for free space in the data dir, hugepages and address-pool
  collisions.
