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
class CreateTimings:
    total_ms: float
    disk_ms: float
    restore_ms: float
    resume_ms: float
    agent_ms: float
    identity_ms: float


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
                m.timings.resume_ms, m.timings.agent_ms, m.timings.identity_ms,
            )
        return cls(
            id=m.id, template=m.template, template_id=m.template_id, state=m.state, pid=m.pid,
            created_at=datetime.fromtimestamp(m.created_at_unix_ms / 1000, tz=timezone.utc),
            ttl=m.ttl_s or None, parent=m.parent, timings=t, _client=client,
        )

    @classmethod
    def create(cls, template: str = "python", ttl: int | None = None, *, client: Client | None = None) -> Sandbox:
        """Restore a new sandbox from a template. ``ttl`` is in seconds."""
        c = client or default_client()
        try:
            m = c.stub.CreateSandbox(pb.CreateSandboxRequest(template=template, ttl_s=ttl or 0), metadata=c.metadata)
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
