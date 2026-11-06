# Changelog

All notable changes to Forkbox. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [0.1.0] - 2026-11-06

First release: boot, guest agent, templates, create from template, daemon
and Python SDK. Single host, x86_64, Linux guests.

### Added

- `forkbox doctor`: 11 host checks (KVM access, kernel, cgroup v2,
  userfaultfd, data directory on XFS or btrfs with a real reflink probe,
  Firecracker and jailer checksums, nftables, iproute2), each failure with
  the command that fixes it.
- `forkbox setup`: downloads the Firecracker v1.17.0 release and the guest
  kernel 6.18.56 pinned in `versions.yaml` and verifies every SHA-256.
- VM manager (`internal/vm`): every VM runs under the jailer, in its own
  chroot with identical relative paths, as an unprivileged uid, with
  Firecracker's default seccomp filter, in its own cgroup v2 group with
  `cpu.max` and `memory.max`. Boot, pause, resume, full and diff snapshots
  (dirty-page tracking on), restore, stop, and a reaper that needs only the
  VM id. Every step is timed.
- Guest agent (`forkbox-agent`), a static binary on vsock port 5250: exec
  with streamed output, timeouts and signals, file read and write with
  SHA-256, stat, wait-for-port, guest info. One operation per connection, so
  clients carry on after a snapshot or restore. Runs under systemd or as
  PID 1 in images without an init system.
- Templates: `forkbox templates build|list|inspect|rm`. A template is an OCI
  image (pulled and flattened in pure Go), apt and pip packages, files and
  the agent, booted once to run warm-up commands, then captured as a full
  snapshot. Content-addressed and read-only once built. Ships
  `templates/python.yaml` and `templates/node.yaml`.
- Sandboxes: restored from a template's snapshot with a reflinked disk and
  the template's memory file mapped copy-on-write, so unchanged pages are
  shared. On restore the agent reseeds the kernel RNG with host entropy,
  writes a new machine-id, sets the hostname to the sandbox id and steps the
  clock to the host's. State in SQLite (WAL).
- `forkboxd`: gRPC API on an owner-only Unix socket, optional TCP listeners
  with a bearer token, an HTTP gateway with server-sent events for exec,
  per-sandbox TTLs, and a startup reaper that adopts live sandboxes and
  removes the rest.
- Python SDK (`sdk/python`, package `forkbox`): `Sandbox.create`, `exec`,
  `exec_stream`, `fs.read`, `fs.write`, `fs.upload`, `destroy`, context
  manager, typed results and errors.
- `bench/restore.py`: create-to-first-exec latency and Firecracker memory.
- CI runs every integration test on real Firecracker microVMs on GitHub's
  KVM-capable runners.

[0.1.0]: https://github.com/Lazypilottt/forkbox/releases/tag/v0.1.0
