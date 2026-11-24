"""Sandboxes: create, exec, files, destroy."""

from __future__ import annotations

import hashlib
import os
from collections.abc import Iterator, Sequence
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Literal

import grpc

from ._client import Client, default_client
from ._proto import forkbox_pb2 as pb
from .errors import CommandError, ForkboxError, from_rpc

CHUNK = 256 << 10


@dataclass(frozen=True)
class ExecResult:
    """The outcome of :meth:`Sandbox.exec`."""

    stdout_bytes: bytes
    stderr_bytes: bytes
    exit_code: int
    signal: str = ""
    timed_out: bool = False
    duration_ms: float = 0.0

    @property
    def stdout(self) -> str:
        return self.stdout_bytes.decode("utf-8", errors="replace")

    @property
    def stderr(self) -> str:
        return self.stderr_bytes.decode("utf-8", errors="replace")

    @property
    def ok(self) -> bool:
        return self.exit_code == 0 and not self.timed_out


@dataclass(frozen=True)
class ExecEvent:
    """One event of :meth:`Sandbox.exec_stream`."""

    kind: Literal["started", "stdout", "stderr", "exit"]
    data: bytes = b""
    exec_id: str = ""
    pid: int = 0
    result: ExecResult | None = None  # set on "exit" (output fields empty)


@dataclass(frozen=True)
class ForkTimings:
    """Measured steps of :meth:`Sandbox.fork`, in milliseconds and bytes."""

    pause_ms: float
    snapshot_ms: float
    disks_ms: float
    resume_ms: float
    parent_paused_ms: float
    merge_ms: float
    restore_ms: tuple[float, ...]
    total_ms: float
    diff_bytes: int
    dirty_bytes: int
    layer: str


@dataclass(frozen=True)
class Checkpoint:
    """A named point a sandbox can be rewound to (memory and disk)."""

    id: str
    sandbox_id: str
    name: str
    layer: str
    created_at: datetime
    paused_ms: float = 0.0  # set on the checkpoint just taken
    dirty_bytes: int = 0

    @classmethod
    def _from_pb(cls, m: pb.Checkpoint) -> Checkpoint:
        return cls(m.id, m.sandbox_id, m.name, m.layer,
                   datetime.fromtimestamp(m.created_at_unix_ms / 1000, tz=timezone.utc), m.paused_ms, m.dirty_bytes)


@dataclass(frozen=True)
class Limits:
    """Per-sandbox resource limits; ``None`` means no limit. Forks inherit them."""

    cpus: float | None = None        # host CPU time, in CPUs (0.5: half of one)
    memory_mib: int | None = None    # host memory for the VM, at least the guest's + 64
    pids_max: int | None = None      # processes started through exec, with their children

    def _pb(self) -> pb.Limits | None:
        if self == Limits():
            return None
        return pb.Limits(cpus=self.cpus or 0, memory_mib=self.memory_mib or 0, pids_max=self.pids_max or 0)

    @classmethod
    def _from_pb(cls, m: pb.Limits) -> Limits:
        return cls(m.cpus or None, m.memory_mib or None, m.pids_max or None)


def _create_request(template: str, ttl: int | None, cpus, memory_mib, pids_max, egress) -> pb.CreateSandboxRequest:
    if isinstance(egress, str):
        egress = [egress]
    return pb.CreateSandboxRequest(template=template, ttl_s=ttl or 0,
                                   limits=Limits(cpus, memory_mib, pids_max)._pb(), egress=list(egress or ()))


@dataclass(frozen=True)
class CreateTimings:
    total_ms: float
    disk_ms: float
    restore_ms: float
    resume_ms: float
    agent_ms: float
    identity_ms: float
    from_pool: bool = False  # a warm pool's paused sandbox was resumed


def _argv(cmd: str | Sequence[str]) -> list[str]:
    if isinstance(cmd, str):
        return ["/bin/sh", "-c", cmd]
    argv = list(cmd)
    if not argv:
        raise ValueError("empty command")
    return argv


class FileSystem:
    """Files in a sandbox (``sb.fs``). Paths are absolute guest paths."""

    def __init__(self, sandbox: Sandbox) -> None:
        self._sb = sandbox

    def read(self, path: str) -> bytes:
        """Read a whole file. The daemon's SHA-256 is checked."""
        c = self._sb._client
        buf = bytearray()
        digest: bytes = b""
        try:
            for ch in c.stub.ReadFile(
                pb.ReadFileRequest(sandbox_id=self._sb.id, path=path), metadata=c.metadata
            ):
                buf += ch.data
                if ch.last:
                    digest = ch.sha256
        except grpc.RpcError as e:
            raise from_rpc(e) from None
        if digest and hashlib.sha256(buf).digest() != digest:
            raise ForkboxError(f"checksum mismatch reading {path}", "DATA_LOSS")
        return bytes(buf)

    def read_text(self, path: str, encoding: str = "utf-8") -> str:
        return self.read(path).decode(encoding)

    def write(
        self,
        path: str,
        data: bytes | str,
        *,
        mode: int = 0o644,
        make_parents: bool = True,
    ) -> int:
        """Write a file; it appears only once complete. Returns its size."""
        if isinstance(data, str):
            data = data.encode()
        resp = self._write_chunks(path, (data[i : i + CHUNK] for i in range(0, len(data), CHUNK)), mode, make_parents)
        _check(resp.sha256, hashlib.sha256(data).digest(), path)
        return resp.size

    def upload(self, local_path: str | os.PathLike[str], remote_path: str, *, mode: int | None = None) -> int:
        """Stream a local file into the sandbox. Returns its size."""
        h = hashlib.sha256()
        if mode is None:
            mode = os.stat(local_path).st_mode & 0o7777

        def chunks() -> Iterator[bytes]:
            with open(local_path, "rb") as f:
                while block := f.read(CHUNK):
                    h.update(block)
                    yield block

        resp = self._write_chunks(remote_path, chunks(), mode, True)
        _check(resp.sha256, h.digest(), remote_path)
        return resp.size

    def _write_chunks(self, path: str, chunks: Iterator[bytes], mode: int, make_parents: bool) -> pb.WriteFileResponse:
        c = self._sb._client

        def requests() -> Iterator[pb.WriteFileRequest]:
            yield pb.WriteFileRequest(
                header=pb.WriteFileHeader(sandbox_id=self._sb.id, path=path, mode=mode, make_parents=make_parents)
            )
            for ch in chunks:
                yield pb.WriteFileRequest(data=ch)

        try:
            return c.stub.WriteFile(requests(), metadata=c.metadata)
        except grpc.RpcError as e:
            raise from_rpc(e) from None


def _check(got: bytes, want: bytes, path: str) -> None:
    if got != want:
        raise ForkboxError(f"checksum mismatch writing {path}", "DATA_LOSS")


@dataclass
class Sandbox:
    """A Forkbox sandbox. Create one with :meth:`create`; use it as a context
    manager to destroy it on exit."""

    id: str
    template: str
    template_id: str
    state: str
    pid: int
    created_at: datetime
    ttl: int | None
    parent: str = ""
    timings: CreateTimings | None = None
    last_fork: ForkTimings | None = None
    # Page server counters (faults, copied, zeroed, fault_p50_us, ...) for a
    # sandbox whose memory is served from a layer chain; from get().
    pager: dict[str, float] | None = None
    limits: Limits | None = None
    error: str = ""  # why it failed, when state is "failed"
    egress: tuple[str, ...] = ()  # the egress allow-list in force
    _client: Client = field(default_factory=default_client, repr=False, compare=False)

    @property
    def fs(self) -> FileSystem:
        return FileSystem(self)

    @classmethod
    def _from_pb(cls, m: pb.Sandbox, client: Client) -> Sandbox:
        t = None
        if m.HasField("timings"):
            t = CreateTimings(
                m.timings.total_ms, m.timings.disk_ms, m.timings.restore_ms,
                m.timings.resume_ms, m.timings.agent_ms, m.timings.identity_ms, m.timings.from_pool,
            )
        return cls(
            id=m.id, template=m.template, template_id=m.template_id, state=m.state, pid=m.pid,
            created_at=datetime.fromtimestamp(m.created_at_unix_ms / 1000, tz=timezone.utc),
            ttl=m.ttl_s or None, parent=m.parent, timings=t, _client=client,
            pager={f.name: v for f, v in m.pager.ListFields()} if m.HasField("pager") else None,
            limits=Limits._from_pb(m.limits) if m.HasField("limits") else None, error=m.error,
            egress=tuple(m.egress),
        )

    @classmethod
    def create(cls, template: str = "python", ttl: int | None = None, *, cpus: float | None = None,
               memory_mib: int | None = None, pids_max: int | None = None,
               egress: Sequence[str] | str | None = None, client: Client | None = None) -> Sandbox:
        """Restore a new sandbox from a template. ``ttl`` is in seconds.

        ``cpus``, ``memory_mib`` and ``pids_max`` cap the sandbox (see
        :class:`Limits`). When the host is at its density limit this raises
        :class:`ResourceExhaustedError` at once.

        Egress is denied by default. ``egress`` adds to the template's
        allow-list: domains (``"pypi.org"`` also allows its subdomains),
        IPv4 addresses or CIDRs, or ``"*"`` for anything. Forks inherit it."""
        c = client or default_client()
        try:
            m = c.stub.CreateSandbox(_create_request(template, ttl, cpus, memory_mib, pids_max, egress),
                                     metadata=c.metadata)
        except grpc.RpcError as e:
            raise from_rpc(e) from None
        return cls._from_pb(m, c)

    @classmethod
    def get(cls, sandbox_id: str, *, client: Client | None = None) -> Sandbox:
        c = client or default_client()
        try:
            return cls._from_pb(c.stub.GetSandbox(pb.GetSandboxRequest(id=sandbox_id), metadata=c.metadata), c)
        except grpc.RpcError as e:
            raise from_rpc(e) from None

    @classmethod
    def list(cls, *, client: Client | None = None) -> list[Sandbox]:
        c = client or default_client()
        try:
            resp = c.stub.ListSandboxes(pb.ListSandboxesRequest(), metadata=c.metadata)
        except grpc.RpcError as e:
            raise from_rpc(e) from None
        return [cls._from_pb(m, c) for m in resp.sandboxes]

    def _exec_events(self, cmd, env, cwd, timeout) -> Iterator[pb.ExecEvent]:
        envs = [f"{k}={v}" for k, v in (env or {}).items()]
        req = pb.ExecRequest(
            sandbox_id=self.id, args=_argv(cmd), env=envs, cwd=cwd or "",
            timeout_ms=int(timeout * 1000) if timeout else 0,
        )
        return self._client.stub.Exec(req, metadata=self._client.metadata)

    def exec_stream(
        self,
        cmd: str | Sequence[str],
        *,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
        timeout: float | None = None,
    ) -> Iterator[ExecEvent]:
        """Run a command and yield its output as it arrives. A string runs
        through ``/bin/sh -c``; a list is the argv. Stopping the iteration
        early kills the command."""
        call = self._exec_events(cmd, env, cwd, timeout)
        try:
            for ev in call:
                which = ev.WhichOneof("event")
                if which == "started":
                    yield ExecEvent("started", exec_id=ev.started.exec_id, pid=ev.started.pid)
                elif which == "stdout":
                    yield ExecEvent("stdout", data=ev.stdout)
                elif which == "stderr":
                    yield ExecEvent("stderr", data=ev.stderr)
                elif which == "exit":
                    x = ev.exit
                    yield ExecEvent("exit", result=ExecResult(b"", b"", x.exit_code, x.signal, x.timed_out, x.duration_ms))
        except grpc.RpcError as e:
            if e.code() == grpc.StatusCode.CANCELLED:
                return
            raise from_rpc(e) from None
        finally:
            call.cancel()

    def exec(
        self,
        cmd: str | Sequence[str],
        *,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
        timeout: float | None = None,
        check: bool = False,
    ) -> ExecResult:
        """Run a command to completion. With ``check=True`` a non-zero exit
        raises :class:`CommandError`."""
        out, err = bytearray(), bytearray()
        result: ExecResult | None = None
        for ev in self.exec_stream(cmd, env=env, cwd=cwd, timeout=timeout):
            if ev.kind == "stdout":
                out += ev.data
            elif ev.kind == "stderr":
                err += ev.data
            elif ev.kind == "exit" and ev.result is not None:
                r = ev.result
                result = ExecResult(bytes(out), bytes(err), r.exit_code, r.signal, r.timed_out, r.duration_ms)
        if result is None:
            raise ForkboxError("exec ended without an exit status", "INTERNAL")
        if check and not result.ok:
            raise CommandError(f"command failed with exit code {result.exit_code}: {result.stderr.strip()[:500]}", result)
        return result

    def fork(self, n: int, *, concurrency: int = 0, memory_backend: str | None = None, prefetch: bool | None = None) -> list[Sandbox]:
        """Fork this running sandbox into ``n`` children. Each continues from
        this sandbox's memory, processes and files at the moment of the fork;
        this sandbox keeps running. Timings are kept in ``last_fork``.

        ``memory_backend`` overrides the daemon's setting: ``"merge"`` merges
        the parent's dirty pages into a shared memory file, ``"uffd"`` keeps
        them as a layer that a page server serves from (no copying).
        ``prefetch`` (uffd) copies those pages before the child asks."""
        req = pb.ForkSandboxRequest(id=self.id, n=n, concurrency=concurrency, memory_backend=memory_backend or "")
        if prefetch is not None:
            req.prefetch = prefetch
        try:
            r = self._client.stub.ForkSandbox(req, metadata=self._client.metadata)
        except grpc.RpcError as e:
            raise from_rpc(e) from None
        t = r.timings
        self.last_fork = ForkTimings(
            t.pause_ms, t.snapshot_ms, t.disks_ms, t.resume_ms, t.parent_paused_ms, t.merge_ms,
            tuple(t.restore_ms), t.total_ms, t.diff_bytes, t.dirty_bytes, r.layer,
        )
        return [Sandbox._from_pb(m, self._client) for m in r.children]

    def _call(self, method: str, req):
        try:
            return getattr(self._client.stub, method)(req, metadata=self._client.metadata)
        except grpc.RpcError as e:
            raise from_rpc(e) from None

    def checkpoint(self, name: str) -> Checkpoint:
        """Save this sandbox's memory, processes and disk under ``name``. The
        sandbox pauses only for the snapshot and keeps running."""
        return Checkpoint._from_pb(self._call("CreateCheckpoint", pb.CreateCheckpointRequest(sandbox_id=self.id, name=name)))

    def checkpoints(self) -> list[Checkpoint]:
        """This sandbox's checkpoints, oldest first."""
        r = self._call("ListCheckpoints", pb.ListCheckpointsRequest(sandbox_id=self.id))
        return [Checkpoint._from_pb(c) for c in r.checkpoints]

    def rewind(self, name: str) -> Sandbox:
        """Go back to a checkpoint: same id, network and port forwards; the
        memory, processes and disk are the checkpoint's."""
        m = self._call("RewindSandbox", pb.RewindSandboxRequest(sandbox_id=self.id, name=name))
        self.state, self.pid = m.state, m.pid
        return self

    def restore_as_new(self, name: str, *, ttl: int | None = None) -> Sandbox:
        """Create a separate sandbox from one of this sandbox's checkpoints."""
        m = self._call("RestoreCheckpoint", pb.RestoreCheckpointRequest(sandbox_id=self.id, name=name, ttl_s=ttl or 0))
        return Sandbox._from_pb(m, self._client)

    def delete_checkpoint(self, name: str) -> None:
        self._call("DeleteCheckpoint", pb.DeleteCheckpointRequest(sandbox_id=self.id, name=name))

    def race(self, n: int, run, check, timeout: float | None = None) -> RaceResult:
        """Fork into ``n`` children and call ``run(child, i)`` in each at
        once. As attempts finish, ``check(child)`` decides; the first child
        that passes is returned and every other child is destroyed, which
        stops its work. The report lists every attempt. With no winner (or
        after ``timeout`` seconds) all children are destroyed."""
        return _sandbox_race(self, n, run, check, timeout)

    def map(self, n: int, run, *, timeout: float | None = None, keep: bool = False) -> list[Attempt]:
        """Fork into ``n`` children, call ``run(child, i)`` in each at once and
        return every attempt with its result. Children are destroyed after,
        unless ``keep``."""
        return _sandbox_map(self, n, run, timeout=timeout, keep=keep)

    def expose(self, port: int, host_port: int = 0) -> tuple[str, int]:
        """Forward a host TCP port to ``port`` in the guest (for previews).
        Returns the ``(host, port)`` the forward listens on."""
        try:
            r = self._client.stub.ExposePort(
                pb.ExposePortRequest(sandbox_id=self.id, guest_port=port, host_port=host_port), metadata=self._client.metadata
            )
        except grpc.RpcError as e:
            raise from_rpc(e) from None
        return r.host, r.host_port

    def destroy(self) -> None:
        """Stop the sandbox and remove everything it used."""
        try:
            self._client.stub.DestroySandbox(pb.DestroySandboxRequest(id=self.id), metadata=self._client.metadata)
        except grpc.RpcError as e:
            if e.code() == grpc.StatusCode.NOT_FOUND:
                return
            raise from_rpc(e) from None
        self.state = "destroyed"

    def refresh(self) -> Sandbox:
        """Re-read the sandbox's state from the daemon."""
        fresh = Sandbox.get(self.id, client=self._client)
        self.state, self.pid = fresh.state, fresh.pid
        return self

    def __enter__(self) -> Sandbox:
        return self

    def __exit__(self, *exc: object) -> None:
        self.destroy()


# ---------------------------------------------------------------- race / map


@dataclass(frozen=True)
class Attempt:
    """One child's attempt in :meth:`Sandbox.race` or :meth:`Sandbox.map`."""

    index: int
    sandbox_id: str
    duration_s: float
    passed: bool = False
    result: object = None
    error: str | None = None  # the exception run() or check() raised
    cancelled: bool = False  # stopped because another attempt won (or timeout)


@dataclass
class RaceResult:
    winner: Sandbox | None
    index: int | None
    attempts: list[Attempt]
    fork: ForkTimings | None = None


def _race(parent: Sandbox, n: int, run, check, timeout: float | None, keep_all: bool) -> RaceResult:
    import concurrent.futures as cf
    import time

    kids = parent.fork(n)
    started = time.monotonic()
    attempts: list[Attempt | None] = [None] * n

    def attempt(i: int) -> tuple[object, str | None, float]:
        t0 = time.monotonic()
        try:
            return run(kids[i], i), None, time.monotonic() - t0
        except Exception as e:  # noqa: BLE001 - reported per attempt
            return None, f"{type(e).__name__}: {e}", time.monotonic() - t0

    winner: int | None = None
    pool = cf.ThreadPoolExecutor(max_workers=n)
    futs = {pool.submit(attempt, i): i for i in range(n)}
    try:
        for fut in cf.as_completed(futs, timeout=timeout):
            i = futs[fut]
            result, err, dur = fut.result()
            passed = False
            if err is None and check is not None:
                try:
                    passed = bool(check(kids[i]))
                except Exception as e:  # noqa: BLE001
                    err = f"check: {type(e).__name__}: {e}"
            attempts[i] = Attempt(i, kids[i].id, dur, passed, result, err)
            if passed and not keep_all:
                winner = i
                break
    except cf.TimeoutError:
        pass
    finally:
        if not keep_all:
            # Destroying a child ends its running work: its exec streams fail.
            losers = [k for j, k in enumerate(kids) if j != winner]
            list(cf.ThreadPoolExecutor(max_workers=max(1, len(losers))).map(lambda k: k.destroy(), losers))
        pool.shutdown(wait=True)
    for i in range(n):
        if attempts[i] is None:
            attempts[i] = Attempt(i, kids[i].id, time.monotonic() - started, cancelled=True)
    return RaceResult(kids[winner] if winner is not None else None, winner, attempts, parent.last_fork)


def _sandbox_race(self: Sandbox, n: int, run, check, timeout: float | None = None) -> RaceResult:
    """Fork into ``n`` children and call ``run(child, i)`` in each at once.
    As attempts finish, ``check(child)`` decides; the first child that passes
    is returned and every other child is destroyed (stopping its work). The
    report lists every attempt: duration, passed, error, cancelled. With no
    winner (or after ``timeout`` seconds) all children are destroyed."""
    if check is None:
        raise ValueError("race needs a check")
    return _race(self, n, run, check, timeout, keep_all=False)


def _sandbox_map(self: Sandbox, n: int, run, *, timeout: float | None = None, keep: bool = False) -> list[Attempt]:
    """Fork into ``n`` children, call ``run(child, i)`` in each at once and
    return every attempt with its result. Children are destroyed afterwards
    unless ``keep`` (then ``Sandbox.get(attempt.sandbox_id)`` finds them)."""
    r = _race(self, n, run, None, timeout, keep_all=True)
    if not keep:
        for a in r.attempts:
            try:
                Sandbox.get(a.sandbox_id, client=self._client).destroy()
            except ForkboxError:
                pass
    return r.attempts



def pool_stats(client: Client | None = None) -> list[dict[str, object]]:
    """The daemon's warm pools: ready, target, hits, misses, demand."""
    c = client or default_client()
    try:
        r = c.stub.PoolStats(pb.PoolStatsRequest(), metadata=c.metadata)
    except grpc.RpcError as e:
        raise from_rpc(e) from None
    return [{f.name: v for f, v in p.ListFields()} | {"template": p.template} for p in r.pools]
