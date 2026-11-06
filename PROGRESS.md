# Forkbox progress

| Task | Status |
| --- | --- |
| 1. Scaffold, `forkbox doctor` and `forkbox setup` | **Done.** Accepted on a KVM host (CI run 42142883146). |
| 2. VM manager: boot, pause, snapshot, restore | **Done.** Accepted on a KVM host (CI run 42142883146). |
| 3. Guest agent over vsock | **Done.** Accepted on a KVM host (CI run 42142883146). |
| 4–17 | Not started. |

**Resume point:** Task 4. Every push runs the integration tests on real
Firecracker microVMs (see "KVM host" below).

## KVM host

The integration job in `.github/workflows/ci.yml` runs on GitHub's hosted
`ubuntu-24.04` runners, which expose hardware virtualization. The job:

- grants access to `/dev/kvm` with a udev rule;
- installs `squashfs-tools`, `e2fsprogs`, `xfsprogs` and `nftables`;
- mounts a 24 GB sparse loop file formatted XFS (`-m reflink=1`) at
  `/var/lib/forkbox`;
- runs `forkbox setup`, `forkbox doctor`, `make integration` (verbose), and
  the container check.

It can also be started by hand: `gh workflow run ci`.

Runner as of 2026-11-06:

- 2 vCPUs, 7.7 GB RAM, Ubuntu 24.04.5, host kernel 7.0.0-1017-azure.
- It is an Azure VM, so microVMs run under **nested virtualization**.
- Host kernel 7.0 is newer than the host kernels Firecracker v1.17 validates.

Timings from these runners are fine for regressions but are **not**
benchmark numbers for the README. Task 6 and Task 16 benchmarks need a
bare-metal or larger host, recorded with the Firecracker version.

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
- **On the KVM host (CI run 42142883146):**
  - `forkbox setup` installed and verified all pins;
  - `forkbox doctor` reported 11 passed, 0 failed, and `TestDoctorPassesOnThisHost`
    passed;
  - the Linux-only storage tests passed;
  - in an `ubuntu:24.04` container without `/dev/kvm`, doctor exited 1 with
    `FAIL kvm /dev/kvm does not exist inside this container` and the fix
    `docker run --device /dev/kvm ...`.

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
- **On the KVM host (CI run 42142883146):** all three integration tests passed.
  - Boot to InstanceStart took 29.8 ms: jail 12.5, configure 1.4, start 15.9.
    The console reached `fbx-test-init: started`. Firecracker ran as uid/gid
    70000 with `Seccomp: 2` in cgroup `/forkbox/<id>`.
  - Snapshot test: pause 0.7 ms, full snapshot of 256 MiB 1093 ms, restore
    17.9 ms (jail 11.5, load 6.4), resume 6.8 ms. The counter was 10 at
    snapshot and the first line after restore was 11, so the guest resumed
    rather than rebooted.
  - Bad kernel, SIGKILLed Firecracker and lost owner all left no process,
    jail, run dir or cgroup.
  - These are nested-virtualization numbers on 2 vCPUs (see "KVM host").

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

## Task 3. Guest agent over vsock (2026-11-06)

### What the docs say (Firecracker v1.17.0)

- **Host-initiated connections** (`docs/vsock.md`): connect to the VM's vsock
  Unix socket, send `CONNECT <port>\n`, and read `OK <host port>\n`. If nothing in
  the guest listens on the port, Firecracker closes the connection.
- **Snapshots** (`snapshot-support.md`, "Vsock device reset"): snapshot creation
  sends a transport reset, and on resume the guest driver closes every
  existing connection. Listening sockets survive.

### Design

- **One operation per connection.** The host connects, sends one `Request`,
  and reads `Response`s until a terminal one. No state lives on a
  connection, so after a snapshot or a restore the next call simply dials
  again. Concurrent Execs are independent connections.
- **Framing:** a 4-byte big-endian length, then a protobuf message
  (`proto/agent.proto`, generated into `proto/agentpb`). Frames over 1 MiB are
  refused on both sides. File and output data travel in 256 KiB chunks.
- **Bounded memory:**
  - **Guest:** at most 8 output frames wait for the connection. When the host
    reads slowly, the agent stops reading the pipes and the process blocks on
    write.
  - **Host:** `Exec`/`Wait` hand each chunk to a callback and buffer nothing.
    `Run` collects at most 16 MiB per stream (configurable) and drops the rest
    while still draining, so the process finishes.
- **Process lifetime:**
  - Each Exec runs in its own process group. A timeout or a lost connection
    sends SIGKILL to the whole group.
  - `Signal` targets the group, by exec id.
  - Output from children that outlive the main process is read for 250 ms
    after it exits.

### What was built

- **`proto/agent.proto`:** the operations from the task: Ping, Exec, Signal,
  ReadFile, WriteFile, Stat, WaitPort, Info.
- **`internal/agentwire`:** port (5250), protocol version, framing.
- **`internal/agent`:** the guest server.
  - **Exec:** argv, env, cwd, timeout, streamed stdout and stderr; exit code or
    signal; a timed-out flag.
  - **Files:** ReadFile streams the file with a SHA-256 at the end. WriteFile
    goes to a temporary file that is renamed into place only once complete,
    and reports a SHA-256. Paths must be absolute.
  - **WaitPort:** reads `/proc/net/tcp{,6}` rather than connecting, so it never
    disturbs the server it waits for.
  - **Info:** hostname, `/etc/machine-id`, boot_id, uptime, realtime clock.
  - **Listener:** an AF_VSOCK listener built on non-blocking `os.File`s (Go's
    `net` package has no vsock support).
  - **Start-up:** `SystemdUnit` starts the agent early in boot (sysinit) and
    restarts it if it dies. `KillMode=process` keeps exec'd processes alive
    across an agent restart.
- **`cmd/forkbox-agent`:** a static binary (5.3 MB), built with `make agent`
  as `bin/forkbox-agent`.
- **`internal/vm/agentclient`:** the host client.
  - `FirecrackerDialer` does the CONNECT handshake; a closed connection there
    is `ErrNotListening`.
  - Every connection attempt retries with backoff (10 ms doubling to 500 ms)
    until the context ends. `WaitReady` also retries requests that drop
    mid-way.
  - Operations: `Ping`, `Start`/`Wait`, `Exec`, `Run`, `Signal`, `ReadFile`,
    `WriteFile` (both check the SHA-256 against the agent's), `Stat`,
    `WaitPort`, `Info`.
  - Agent errors map to `fs.ErrNotExist`, `fs.ErrPermission` and
    `fs.ErrInvalid`.
- **Build:** a `make proto` target, and `make integration` now builds the
  agent first. CI's integration step runs `make integration`.

### Files

`proto/agent.proto`, `proto/agentpb/agent.pb.go`, `internal/agentwire/*`,
`internal/agent/*`, `cmd/forkbox-agent/main.go`, `internal/vm/agentclient/*`,
`internal/vm/agent_integration_test.go`, `internal/vm/vm_integration_test.go`
(shared image builder), `Makefile`, `.github/workflows/ci.yml`, `go.mod`
(google.golang.org/protobuf v1.36.6).

### How to try it on the KVM host

```sh
make integration      # builds bin/forkbox-agent, then runs every integration test
# only this task:
make agent && go test -tags integration -count=1 -exec 'sudo -n' -v -run IntegrationAgent ./internal/vm/
```

`TestIntegrationAgent` builds an image from the pinned rootfs plus the agent
and its enabled unit, and boots a reflinked writable copy with systemd as
init. It then checks:

- echo;
- 10 streamed lines, the first arriving within 1 s;
- a 500 ms timeout on `sleep 30`;
- a 50 MiB write and read-back with matching SHA-256 (times logged);
- pause, full snapshot and resume, after which the same client works again;
- stop, restore into a new jail with the disk as of the snapshot, and
  resume. A client for the new socket then gets the same boot_id and
  machine-id and the same 50 MiB file, and exec works;
- no leftovers.

### Test results (macOS arm64, Go 1.27.0)

- gofmt, `go vet` and staticcheck pass for darwin and linux/amd64, with and
  without `integration`.
- `go test -race ./...`: 137 passed, 0 failed (21 new). The agentclient tests
  run the real agent server behind a fake of Firecracker's vsock socket (the
  CONNECT handshake, refusing connections while "not listening"). They cover:
  - retry until the agent listens, and giving up at the deadline;
  - echo; env, cwd, stderr and exit code; streaming before exit;
  - a timeout that kills a background child too;
  - signals; 32 concurrent execs;
  - closing the stream kills the process;
  - a 20 MiB output capped at 1 MiB;
  - a 20 MiB file round trip, file errors, an aborted upload leaving no file;
  - WaitPort; Info; a 1 GiB frame refused.
- Mutation checks: signalling only the main pid fails the timeout test (its
  child survives), and dropping the disconnect watcher fails the
  close-the-stream test.
- **On the KVM host (CI run 42142883146):** `TestIntegrationAgent` passed.
  - The agent answered 2.19 s after InstanceStart (systemd boot).
  - echo, 10 streamed lines and the 500 ms timeout behaved as expected.
  - 50 MiB went up in 439 ms and down in 342 ms, with checksums matching.
  - After pause, snapshot and resume, the same client was back in 15 ms.
  - After a restore into a new jail, a new client got an answer 60 ms after
    resume, with the same boot_id and machine-id and the 50 MiB file intact.
- **Bug found by the first KVM run (run 42139491026), fixed in `56d7653`.**
  When the agent refused an upload at once, the client hit a broken pipe,
  hung up, and lost the agent's error frame, so the caller saw "broken pipe"
  instead of `fs.ErrNotExist`. The client now reads the agent's answer before
  hanging up, unless its own source failed. `TestFileErrors` now uses a
  32 MiB upload, so the early hang-up reliably happens mid-upload.

### Limitations

- An Exec whose connection breaks is killed, including one cut by a snapshot
  (vsock reset). Long-lived processes should be started detached, e.g.
  `setsid nohup ... &`, until a detached exec exists.
- No stdin for Exec, and no TTY.
- There is no authentication on the vsock port: anything on the host that can
  open the VM's vsock socket is trusted. The socket lives in the root-owned
  jail.
- WaitPort covers TCP only.

### Open questions

- Should an Exec in flight across a fork survive in the parent (Task 8)? Today
  the reset kills it.

---

## Blocked

Nothing.

## TODO (manual)

1. **Before the benchmarks (Tasks 6, 11, 16):** a bare-metal or larger x86_64
   KVM host. The CI runners are nested-virtualized with 2 vCPUs.
   - Ubuntu 24.04, at least 8 vCPUs and 16 GB RAM;
   - XFS (`mkfs.xfs -m reflink=1`) or btrfs at `/var/lib/forkbox`;
   - `squashfs-tools`, `e2fsprogs`.

   Not needed before then.
2. **Actions minutes:** the unit job takes about 30 s and the integration job about 1.5 minutes per push, both
   is billed to the account's private-repo minutes.

## Ideas

- `forkbox doctor --json`.
- Mirror the pinned kernel and rootfs to a Forkbox release.
- Pin aarch64 artifacts.
- Detached Exec (survives disconnects; output to a log file) and stdin/TTY support.
- Per-VM jail uids/gids from a reserved range (Task 14 hardening).
- Try `--enable-pci` (faster virtio) once snapshot support with PCI is confirmed.
- Doctor checks for free space in the data dir, hugepages and address-pool
  collisions.
