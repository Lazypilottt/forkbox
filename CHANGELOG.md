# Changelog

All notable changes to Forkbox. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [0.2.0] - 2026-11-07

Networking, live fork, checkpoints, the clone-correctness suite and the MCP
server.

### Added

- Networking: every sandbox gets its own network namespace with the tap the
  snapshot expects (same name, address and MAC everywhere) and a veth pair
  with a unique /30 from a pool in SQLite. NAT gives each clone a unique
  source address. Egress allows DNS, HTTP, HTTPS and ping, never cloud
  metadata or other sandboxes. Port forwards for previews
  (`sb.expose(port)`). Templates bake the guest network in.
  `forkbox doctor` checks IPv4 forwarding.
- Live fork (`sb.fork(n)`): pause, diff snapshot (no fsync), disk reflinks,
  resume; the diff is merged onto the parent's base memory with
  FICLONERANGE into one shared layer; children are restored in parallel.
  Layer lineage in SQLite. Every step is timed.
- Checkpoints: `sb.checkpoint(name)`, `sb.rewind(name)` (same id, network
  slot and port forwards), `sb.restore_as_new(name)`, `sb.checkpoints()`,
  `sb.delete_checkpoint(name)`. Layers are garbage-collected by reference.
- Clone correctness: the agent writes `/run/forkbox/generation` after every
  restore; the python template's `forkbox_guest` (loaded by sitecustomize)
  reseeds `random`, numpy's global generator and OpenSSL in running
  processes, and runs `on_restore` callbacks. An integration suite forks
  one parent into 50 and checks randomness, identity, clocks, stale TCP
  connections and isolation.
- MCP server (`mcp/`, `forkbox-mcp`): create_sandbox, exec, read_file,
  write_file, fork, checkpoint, rewind, list_sandboxes, destroy, over stdio.
- `bench/fork_curve.py` with cold-boot, full-restore and `docker run`
  baselines (`forkbox bench`).
- Daemon recovery resumes a parent left paused by a fork cut short, and
  removes half-written diffs and unrecorded layers and checkpoints.

### Fixed

- A restarted daemon reaped running sandboxes instead of adopting them (the
  jailer passes `--id <id>` as two arguments); liveness now checks the VM's
  cgroup.
- `SQLITE_BUSY` under parallel restores; the store uses one connection.

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

[0.2.0]: https://github.com/Lazypilottt/forkbox/releases/tag/v0.2.0
[0.1.0]: https://github.com/Lazypilottt/forkbox/releases/tag/v0.1.0
