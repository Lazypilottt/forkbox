# Forkbox progress

| Task | Status |
| --- | --- |
| 1. Scaffold, `forkbox doctor` and `forkbox setup` | **Built; acceptance pending on a KVM host.** All checks that can run on macOS pass. |
| 2. VM manager: boot, pause, snapshot, restore | **Built; acceptance pending on a KVM host.** Unit tests pass; integration tests written and skip cleanly here. |
| 3–17 | Not started. They need a Linux KVM host. |

**Resume point:** on the KVM host, run the acceptance steps for Task 1 and then Task 2 (each entry's "How to try it"). Fix anything that fails, then start Task 3.

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

## Task 2. VM manager: boot, pause, snapshot, restore (2026-11-06)

### Firecracker API calls used (v1.17.0)

Read from `docs/getting-started.md`, `docs/jailer.md`,
`docs/snapshotting/snapshot-support.md` and `src/firecracker/swagger/firecracker.yaml`
at tag v1.17.0, and firecracker-go-sdk's `jailer.go` and `machine.go`.

- **Launch:** `jailer --id <id> --exec-file <data>/bin/firecracker --uid 70000 --gid 70000
  --chroot-base-dir <data>/jail --cgroup-version 2 --parent-cgroup forkbox
  --cgroup cpu.max=<quota> 100000 --cgroup memory.max=<bytes> [--netns <path>]
  -- --api-sock /api.sock`. The jail root is `<data>/jail/firecracker/<id>/root`.
  No `--daemonize` or `--new-pid-ns`, so the jailer execs into Firecracker and
  keeps its pid. Seccomp stays at Firecracker's default filter.
- **Ready:** poll `GET /` until it answers.
- **Boot:**
  - `PUT /machine-config` `{vcpu_count, mem_size_mib, smt: false, track_dirty_pages: true}`
  - `PUT /boot-source` `{kernel_image_path: "/boot/vmlinux", boot_args}`
  - `PUT /drives/rootfs` `{drive_id, path_on_host: "/drives/rootfs", is_root_device: true, is_read_only}`
  - `PUT /vsock` `{guest_cid, uds_path: "/v.sock"}` (optional)
  - `PUT /network-interfaces/{iface_id}` `{iface_id, host_dev_name, guest_mac}` (optional)
  - `PUT /actions` `{action_type: "InstanceStart"}`
- **Pause / resume:** `PATCH /vm` `{state: "Paused" | "Resumed"}`.
- **Snapshot:** `PUT /snapshot/create` `{snapshot_type: "Full" | "Diff",
  snapshot_path: "/snapshot/vmstate", mem_file_path: "/snapshot/mem"}`. The VM must be paused.
- **Restore** (fresh Firecracker, nothing configured): `PUT /snapshot/load`
  `{snapshot_path: "/restore/vmstate", mem_backend: {backend_type: "File",
  backend_path: "/restore/mem"}, track_dirty_pages: true, resume_vm: false}`,
  then `PATCH /vm` Resumed.
- **Errors:** non-2xx answers carry `{fault_message}`.

### What was built

- **`internal/vm`:**
  - `Manager.Boot(ctx, cfg)`, `Manager.Restore(ctx, snapshotDir, memBackend, cfg)`
    and `Manager.Reap(id)`.
  - `VM.Pause`, `Resume`, `SnapshotFull(dir)`, `SnapshotDiff(dir)`, `Stop`.
  - `Config` covers vCPUs, memory, kernel, kernel args, the rootfs drive, vsock,
    the network interface, the network namespace, and optional `cpu.max` /
    `memory.max` overrides.
- **Jail:** every input is hard-linked to the same in-jail path in every VM:
  `/boot/vmlinux`, `/drives/rootfs`, `/restore/{vmstate,mem}`; sockets at
  `/api.sock` and `/v.sock`. `/boot`, `/drives` and `/restore` and their files
  are root-owned, so the jailed uid cannot change them; only a writable rootfs
  and `/snapshot` belong to it.
- **Snapshots:** Firecracker writes into the jail's `/snapshot`; the files are
  then moved (renamed) into the snapshot dir, made `root:<jail gid>` 0640, and
  described by `snapshot.json` (type, Firecracker version, vCPUs, memory).
  Restore links them into `/restore`, a path no snapshot ever writes to:
  Firecracker truncates existing snapshot targets, which would corrupt a
  hard-linked source.
- **Cgroups:** each VM gets `/sys/fs/cgroup/forkbox/<id>`, created by the
  jailer with `cpu.max` (default: vCPUs whole CPUs) and `memory.max` (default:
  guest memory + 64 MiB). Forkbox enables `+cpu +memory` in the parent's
  `cgroup.subtree_control` first.
- **Failure handling:**
  - Every API call has a deadline: 5 s for most, 2 min for snapshot create and
    load, 5 s for the socket to come up.
  - Any failure during Boot or Restore kills the process and removes the jail,
    cgroup and run dir. The returned error includes the last 2 KiB of the
    console.
  - Cleanup (`Stop`, `Reap`): SIGKILL, `cgroup.kill`, wait for the group to
    empty, `rmdir` the group, remove the jail and run dir. `Reap(id)` needs only
    the id, for a future reaper after a daemon restart.
- **Timing:** `VM.Timings()` lists each step's duration: `jail`, `configure`,
  `start`, `boot`, `pause`, `resume`, `snapshot-full`, `snapshot-diff`,
  `restore-jail`, `restore-load`, `restore`, `stop`.
- **`setup.FetchVerified`:** the checksum-verified download, exported for the
  integration test's rootfs.

### Decisions

- **Own API client, not firecracker-go-sdk.** The SDK's snapshot load still
  sends the deprecated `enable_diff_snapshots` rather than v1.17's
  `track_dirty_pages`, and Forkbox needs exact control of jail paths. The
  client is about 150 lines over the pinned swagger. The SDK's jailer flags
  and link-into-chroot approach match what Forkbox does.
- **When a failing call stops a live VM.** The docs say a Firecracker error
  answer to pause, resume or snapshot has no side effects, so on an error
  answer the VM keeps running and the caller gets the error. A timeout or a
  dead socket means the VMM is unhealthy, so the VM is killed and cleaned up.
- **MMIO transport.** Getting-started suggests `--enable-pci`. Its effect on
  snapshots isn't covered in the docs I read, so it is left off for now (see Ideas).

### Files

`internal/vm/{config,layout,api,cgroup,process,manager,vm}.go`,
`internal/vm/{fake_test,vm_test,vm_integration_test}.go`,
`internal/setup/setup.go` (+ test).

### How to try it on the KVM host

```sh
sudo apt install squashfs-tools e2fsprogs   # builds the test rootfs once
sudo bin/forkbox setup
make check
make integration
# or only this task, with timings:
go test -tags integration -count=1 -exec 'sudo -n' -v -run Integration ./internal/vm/
```

The first run downloads the pinned Ubuntu squashfs (checksum-verified) and builds
`/var/lib/forkbox/test/counter-<sha>.ext4`: the rootfs plus `/fbx-test-init`,
a pid-1 shell loop printing `fbx-counter N` to the serial console every 100 ms.

- **Boot test:**
  - the console reaches `fbx-test-init: started`, with the kernel banner;
  - Firecracker runs as uid/gid 70000 with `Seccomp: 2`, in cgroup `/forkbox/<id>`
    with the expected `cpu.max` and `memory.max`;
  - the vsock socket exists.
- **Snapshot test:** boot, wait for counter ≥ 10, pause, full snapshot, stop,
  restore into a new jail, resume. The restored console must not show init
  starting again, and its first counter must be within 2 of the value at
  snapshot time.
- **Failure test:**
  - a zeroed kernel;
  - Firecracker killed with SIGKILL, then an API call;
  - `Reap(id)` with no VM handle.
- **After each test:** no process with `--id=<id>`, no jail, run dir or cgroup.

### Test results (macOS arm64, Go 1.27.0)

- gofmt, `go vet` and staticcheck pass for darwin and linux/amd64, with and
  without the `integration` tag. The Linux integration test binary links.
- `go test -race ./...`: 116 passed, 0 failed (17 new in `internal/vm`, 1 in
  `internal/setup`).
- The vm unit tests run against a fake jailer and Firecracker. The fake
  creates the cgroup and pid file the way the jailer does, serves a
  Firecracker-like API on the jail socket, and can fail, hang or exit early.
  The tests cover:
  - the exact jailer argv;
  - request order and bodies;
  - hard links rather than copies;
  - chown and mode of snapshot files;
  - refusals: wrong state, used snapshot dir, diff snapshot, uffd, version or
    vCPU mismatch, duplicate id, socket path too long;
  - cleanup after an API error, a timeout and an early exit;
  - `Reap` writing `cgroup.kill`.
- A mutation check (turning off `track_dirty_pages`) fails the tests as expected.
- **Not run yet:** the three KVM integration tests.

### Limitations

- `Stop` is a SIGKILL; the guest is not shut down cleanly (a sandbox does not need it).
- One uid/gid (70000) for every jail. Per-VM uids would isolate VMs from each
  other's files; noted under Ideas.
- The `forkbox` parent cgroup sits directly under the cgroup root, outside
  systemd's tree. The daemon (Task 6) should run with `Delegate=yes` and use its
  own subtree instead.
- Restore only accepts full snapshots; diff snapshots must be merged first (Task 8).
- The uffd memory backend returns "not built yet (Task 12)".
- Snapshot dirs and drives must be on the data dir's filesystem; a cross-device
  move fails with a clear error rather than copying gigabytes.

### Open questions

- Whether the jailer enables `+cpu +memory` in the parent group itself; the
  docs don't say. Forkbox enables them first, which works either way.
- Whether `--enable-pci` works with snapshot restore in v1.17. Check before
  turning it on.

---

## Blocked

- The acceptance checks of Tasks 1 and 2, and all work from Task 3 on, need a
  Linux x86_64 KVM host.
  Apple Silicon M1 Macs have no nested virtualisation.

## TODO (manual)

1. **KVM host:**
   - Ubuntu 24.04, at least 8 vCPUs and 16 GB RAM;
   - XFS (`mkfs.xfs -m reflink=1`) or btrfs mounted at `/var/lib/forkbox`.

   Install `squashfs-tools` and `e2fsprogs` (the Task 2 integration test
   builds its rootfs with them). Clone the repo there and run the steps above.
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
- Per-VM jail uids/gids from a reserved range (Task 14 hardening).
- Try `--enable-pci` (faster virtio) once snapshot support with PCI is confirmed.
- Doctor checks for free space in the data dir, hugepages and address-pool
  collisions.
