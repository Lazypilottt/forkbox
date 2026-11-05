# Forkbox

> **Status: early development.** Only `forkbox doctor` and `forkbox setup`
> exist so far. See [PROGRESS.md](PROGRESS.md).

## What it is

Forkbox is an open-source, self-hostable sandbox runtime for AI agents, built on
[Firecracker](https://firecracker-microvm.github.io/) microVMs. Its defining
feature is **fork**: one running sandbox becomes N copies in milliseconds, so an
agent can try many approaches in parallel and keep the one that works.

- **Templates:** build a root filesystem from a short spec, warm it up, and snapshot it.
- **Create:** restore a sandbox from a template snapshot, with copy-on-write memory and disk.
- **Fork, checkpoint, rewind:** branch or roll back a live sandbox.
- **Agent API:** exec, files, `fork(n)`, `race(n, run, check)`, through a Python SDK and an MCP server.

## Requirements

- A Linux x86_64 host with KVM: bare metal, a PC with virtualisation enabled, or
  a cloud VM with nested virtualisation. macOS and Windows cannot run Forkbox.
- Kernel 5.10 or newer with cgroup v2. Ubuntu 24.04 LTS is the reference host.
- A data directory at `/var/lib/forkbox` on **XFS (with reflink) or btrfs**.
- `nftables` and `iproute2`.

Pinned components ([versions.yaml](versions.yaml)):

| Component | Version |
| --- | --- |
| Firecracker and jailer | v1.17.0 |
| Guest kernel | 6.18.56 (Firecracker CI build) |
| Base rootfs | Ubuntu 24.04 (Firecracker CI build) |

## Quick start

So far, only host preparation:

```sh
go build -o forkbox ./cmd/forkbox
sudo ./forkbox setup     # download and verify Firecracker, the jailer and the guest kernel
./forkbox doctor         # one PASS or FAIL line per check, each failure with its fix
```

The sandbox quick start (create, exec, fork) arrives with the Python SDK.

## How forking works

1. Pause the running sandbox.
2. Take a diff snapshot of the memory pages it dirtied.
3. Build child memory: in v0.2, a reflink copy of the base with the dirty pages
   written in; in v1.0, a userfaultfd page server that serves pages from a layer chain.
4. Restore N children in parallel, each with a reflinked disk, its own jail and
   its own network namespace.
5. Resume the parent.

Each child then gets fresh randomness, machine-id, hostname and clock, driven by
Firecracker's VM generation ID.

## Benchmarks

None yet. Every performance claim will come from a benchmark on real microVMs,
with the host details and the Firecracker version recorded alongside.

## Limits

- Single host.
- x86_64 first.
- Linux guests only.
- No GPU passthrough.
- Not security-audited.

## Contributing

Progress is logged in [PROGRESS.md](PROGRESS.md).

```sh
make check          # go vet, staticcheck, unit tests (any OS)
make integration    # integration tests on a KVM host (skip cleanly elsewhere)
```

Licensed under the [Apache License 2.0](LICENSE).
