"""Async Forkbox API (asyncio), alongside the sync one.

    import asyncio
    from forkbox.aio import AsyncSandbox

    async def main():
        async with await AsyncSandbox.create("python") as sb:
            r = await sb.exec("python -c 'print(6 * 7)'")
            print(r.stdout)

    asyncio.run(main())
"""

from __future__ import annotations

import asyncio
import hashlib
import os
import time
from collections.abc import AsyncIterator, Awaitable, Callable, Sequence
from dataclasses import dataclass, field
from datetime import datetime, timezone

import grpc
import grpc.aio

from ._client import DEFAULT_SOCKET
from ._proto import forkbox_pb2 as pb
from ._proto import forkbox_pb2_grpc
from .errors import CommandError, ForkboxError, from_rpc
from .sandbox import CHUNK, Attempt, Checkpoint, CreateTimings, ExecEvent, ExecResult, ForkTimings, _argv


class AsyncClient:
    """An asyncio connection to forkboxd (same settings as forkbox.Client)."""

    def __init__(self, socket: str | None = None, *, address: str | None = None, token: str | None = None) -> None:
        address = address or os.environ.get("FORKBOX_ADDR")
        token = token or os.environ.get("FORKBOX_TOKEN")
        if address:
            self._channel = grpc.aio.insecure_channel(address)
            self.metadata: tuple[tuple[str, str], ...] = (("authorization", f"Bearer {token}"),) if token else ()
        else:
            path = socket or os.environ.get("FORKBOX_SOCKET") or DEFAULT_SOCKET
            self._channel = grpc.aio.insecure_channel(f"unix://{path}")
            self.metadata = ()
        self.stub = forkbox_pb2_grpc.ForkboxStub(self._channel)

    async def close(self) -> None:
        await self._channel.close()

    async def __aenter__(self) -> AsyncClient:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.close()


_default: AsyncClient | None = None


def default_client() -> AsyncClient:
    global _default
    if _default is None:
        _default = AsyncClient()
    return _default


def set_default_client(c: AsyncClient | None) -> None:
    global _default
    _default = c


async def _call(coro):
    try:
        return await coro
    except grpc.aio.AioRpcError as e:
        raise from_rpc(e) from None


class AsyncFileSystem:
    def __init__(self, sb: AsyncSandbox) -> None:
        self._sb = sb

    async def read(self, path: str) -> bytes:
        c = self._sb._client
        buf = bytearray()
        digest = b""
        try:
            async for ch in c.stub.ReadFile(pb.ReadFileRequest(sandbox_id=self._sb.id, path=path), metadata=c.metadata):
                buf += ch.data
                if ch.last:
                    digest = ch.sha256
        except grpc.aio.AioRpcError as e:
            raise from_rpc(e) from None
        if digest and hashlib.sha256(buf).digest() != digest:
            raise ForkboxError(f"checksum mismatch reading {path}", "DATA_LOSS")
        return bytes(buf)

    async def read_text(self, path: str, encoding: str = "utf-8") -> str:
        return (await self.read(path)).decode(encoding)

    async def write(self, path: str, data: bytes | str, *, mode: int = 0o644, make_parents: bool = True) -> int:
        if isinstance(data, str):
            data = data.encode()
        c = self._sb._client

        async def reqs():
            yield pb.WriteFileRequest(header=pb.WriteFileHeader(sandbox_id=self._sb.id, path=path, mode=mode, make_parents=make_parents))
            for i in range(0, len(data), CHUNK):
                yield pb.WriteFileRequest(data=data[i : i + CHUNK])

        r = await _call(c.stub.WriteFile(reqs(), metadata=c.metadata))
        if r.sha256 != hashlib.sha256(data).digest():
            raise ForkboxError(f"checksum mismatch writing {path}", "DATA_LOSS")
        return r.size


@dataclass
class AsyncSandbox:
    """The asyncio twin of forkbox.Sandbox."""

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
    _client: AsyncClient = field(default_factory=default_client, repr=False, compare=False)

    @property
    def fs(self) -> AsyncFileSystem:
        return AsyncFileSystem(self)

    @classmethod
    def _from_pb(cls, m: pb.Sandbox, client: AsyncClient) -> AsyncSandbox:
        t = None
        if m.HasField("timings"):
            t = CreateTimings(m.timings.total_ms, m.timings.disk_ms, m.timings.restore_ms,
                              m.timings.resume_ms, m.timings.agent_ms, m.timings.identity_ms)
        return cls(m.id, m.template, m.template_id, m.state, m.pid,
                   datetime.fromtimestamp(m.created_at_unix_ms / 1000, tz=timezone.utc), m.ttl_s or None, m.parent, t,
                   _client=client)

    @classmethod
    async def create(cls, template: str = "python", ttl: int | None = None, *, client: AsyncClient | None = None) -> AsyncSandbox:
        c = client or default_client()
        m = await _call(c.stub.CreateSandbox(pb.CreateSandboxRequest(template=template, ttl_s=ttl or 0), metadata=c.metadata))
        return cls._from_pb(m, c)

    @classmethod
    async def get(cls, sandbox_id: str, *, client: AsyncClient | None = None) -> AsyncSandbox:
        c = client or default_client()
        return cls._from_pb(await _call(c.stub.GetSandbox(pb.GetSandboxRequest(id=sandbox_id), metadata=c.metadata)), c)

    @classmethod
    async def list(cls, *, client: AsyncClient | None = None) -> list[AsyncSandbox]:
        c = client or default_client()
        r = await _call(c.stub.ListSandboxes(pb.ListSandboxesRequest(), metadata=c.metadata))
        return [cls._from_pb(m, c) for m in r.sandboxes]

    async def exec_stream(self, cmd: str | Sequence[str], *, env: dict[str, str] | None = None,
                          cwd: str | None = None, timeout: float | None = None) -> AsyncIterator[ExecEvent]:
        req = pb.ExecRequest(sandbox_id=self.id, args=_argv(cmd), env=[f"{k}={v}" for k, v in (env or {}).items()],
                             cwd=cwd or "", timeout_ms=int(timeout * 1000) if timeout else 0)
        call = self._client.stub.Exec(req, metadata=self._client.metadata)
        try:
            async for ev in call:
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
        except grpc.aio.AioRpcError as e:
            if e.code() == grpc.StatusCode.CANCELLED:
                return
            raise from_rpc(e) from None
        finally:
            call.cancel()

    async def exec(self, cmd: str | Sequence[str], *, env: dict[str, str] | None = None, cwd: str | None = None,
                   timeout: float | None = None, check: bool = False) -> ExecResult:
        out, err = bytearray(), bytearray()
        result = None
        async for ev in self.exec_stream(cmd, env=env, cwd=cwd, timeout=timeout):
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

    async def fork(self, n: int, *, concurrency: int = 0, memory_backend: str | None = None, prefetch: bool | None = None) -> list[AsyncSandbox]:
        req = pb.ForkSandboxRequest(id=self.id, n=n, concurrency=concurrency, memory_backend=memory_backend or "")
        if prefetch is not None:
            req.prefetch = prefetch
        r = await _call(self._client.stub.ForkSandbox(req, metadata=self._client.metadata))
        t = r.timings
        self.last_fork = ForkTimings(t.pause_ms, t.snapshot_ms, t.disks_ms, t.resume_ms, t.parent_paused_ms, t.merge_ms,
                                     tuple(t.restore_ms), t.total_ms, t.diff_bytes, t.dirty_bytes, r.layer)
        return [AsyncSandbox._from_pb(m, self._client) for m in r.children]

    async def checkpoint(self, name: str) -> Checkpoint:
        m = await _call(self._client.stub.CreateCheckpoint(pb.CreateCheckpointRequest(sandbox_id=self.id, name=name), metadata=self._client.metadata))
        return Checkpoint._from_pb(m)

    async def rewind(self, name: str) -> AsyncSandbox:
        m = await _call(self._client.stub.RewindSandbox(pb.RewindSandboxRequest(sandbox_id=self.id, name=name), metadata=self._client.metadata))
        self.state, self.pid = m.state, m.pid
        return self

    async def expose(self, port: int, host_port: int = 0) -> tuple[str, int]:
        r = await _call(self._client.stub.ExposePort(pb.ExposePortRequest(sandbox_id=self.id, guest_port=port, host_port=host_port), metadata=self._client.metadata))
        return r.host, r.host_port

    async def destroy(self) -> None:
        try:
            await self._client.stub.DestroySandbox(pb.DestroySandboxRequest(id=self.id), metadata=self._client.metadata)
        except grpc.aio.AioRpcError as e:
            if e.code() != grpc.StatusCode.NOT_FOUND:
                raise from_rpc(e) from None
        self.state = "destroyed"

    async def race(self, n: int, run: Callable[[AsyncSandbox, int], Awaitable[object]],
                   check: Callable[[AsyncSandbox], Awaitable[bool]], timeout: float | None = None) -> AsyncRaceResult:
        """Fork into ``n``, run ``await run(child, i)`` in each concurrently,
        ``await check(child)`` as they finish; the first that passes wins and
        the other attempts are cancelled and their children destroyed."""
        return await _arace(self, n, run, check, timeout, keep_all=False)

    async def map(self, n: int, run: Callable[[AsyncSandbox, int], Awaitable[object]], *,
                  timeout: float | None = None, keep: bool = False) -> list[Attempt]:
        r = await _arace(self, n, run, None, timeout, keep_all=True)
        if not keep:
            await asyncio.gather(*(AsyncSandbox(a.sandbox_id, "", "", "", 0, datetime.now(timezone.utc), None,
                                                _client=self._client).destroy() for a in r.attempts), return_exceptions=True)
        return r.attempts

    async def __aenter__(self) -> AsyncSandbox:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.destroy()


@dataclass
class AsyncRaceResult:
    winner: AsyncSandbox | None
    index: int | None
    attempts: list[Attempt]
    fork: ForkTimings | None = None


async def _arace(parent: AsyncSandbox, n: int, run, check, timeout, keep_all: bool) -> AsyncRaceResult:
    kids = await parent.fork(n)
    started = time.monotonic()
    attempts: list[Attempt | None] = [None] * n

    async def attempt(i: int):
        t0 = time.monotonic()
        try:
            return i, await run(kids[i], i), None, time.monotonic() - t0
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001
            return i, None, f"{type(e).__name__}: {e}", time.monotonic() - t0

    tasks = [asyncio.create_task(attempt(i)) for i in range(n)]
    winner = None
    try:
        for fut in asyncio.as_completed(tasks, timeout=timeout):
            i, result, err, dur = await fut
            passed = False
            if err is None and check is not None:
                try:
                    passed = bool(await check(kids[i]))
                except Exception as e:  # noqa: BLE001
                    err = f"check: {type(e).__name__}: {e}"
            attempts[i] = Attempt(i, kids[i].id, dur, passed, result, err)
            if passed and not keep_all:
                winner = i
                break
    except TimeoutError:
        pass
    finally:
        for t in tasks:
            t.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        if not keep_all:
            await asyncio.gather(*(k.destroy() for j, k in enumerate(kids) if j != winner), return_exceptions=True)
    for i in range(n):
        if attempts[i] is None:
            attempts[i] = Attempt(i, kids[i].id, time.monotonic() - started, cancelled=True)
    return AsyncRaceResult(kids[winner] if winner is not None else None, winner, attempts, parent.last_fork)
