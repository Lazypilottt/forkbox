"""SDK unit tests against the fake daemon in conftest.py."""

from __future__ import annotations

import os
import time

import pytest

import forkbox
from forkbox import (
    CommandError,
    ExecResult,
    FailedPreconditionError,
    InvalidArgumentError,
    NotFoundError,
    Sandbox,
)


def test_create_get_list_destroy(daemon):
    sb = Sandbox.create("python", ttl=60)
    assert sb.id == "sb-1" and sb.template == "python" and sb.state == "running" and sb.ttl == 60
    assert sb.timings is not None and sb.timings.total_ms == 80.5
    assert Sandbox.get(sb.id).id == sb.id
    assert [s.id for s in Sandbox.list()] == ["sb-1"]
    sb.destroy()
    assert sb.state == "destroyed"
    with pytest.raises(NotFoundError):
        Sandbox.get("sb-1")
    sb.destroy()  # already gone: not an error


def test_create_errors(daemon):
    with pytest.raises(NotFoundError) as e:
        Sandbox.create("ruby")
    assert e.value.code == "NOT_FOUND" and "ruby" in str(e.value)
    with pytest.raises(InvalidArgumentError):
        Sandbox.create("")


def test_context_manager_destroys(daemon):
    with Sandbox.create() as sb:
        assert sb.id in daemon.sandboxes
    assert sb.id not in daemon.sandboxes


def test_exec(daemon):
    with Sandbox.create() as sb:
        r = sb.exec("echo out; echo err >&2; exit 3", env={"X": "1"})
        assert isinstance(r, ExecResult)
        assert (r.stdout, r.stderr, r.exit_code, r.ok) == ("out\n", "err\n", 3, False)
        r = sb.exec(["sh", "-c", 'echo "$GREETING"'], env={"GREETING": "hello"})
        assert r.stdout == "hello\n" and r.ok
        r = sb.exec("pwd", cwd="/")
        assert r.stdout == "/\n"
        with pytest.raises(CommandError) as e:
            sb.exec("echo nope >&2; exit 1", check=True)
        assert e.value.result.exit_code == 1 and "nope" in str(e.value)
        with pytest.raises(NotFoundError):
            sb.exec(["/no/such/binary"])
        with pytest.raises(ValueError):
            sb.exec([])


def test_exec_timeout(daemon):
    with Sandbox.create() as sb:
        t = time.monotonic()
        r = sb.exec("sleep 30", timeout=0.3)
        assert r.timed_out and r.signal == "killed" and r.exit_code == -1 and not r.ok
        assert time.monotonic() - t < 5


def test_exec_stream_arrives_incrementally(daemon):
    with Sandbox.create() as sb:
        start = time.monotonic()
        first = None
        kinds, out = [], b""
        for ev in sb.exec_stream("for i in 1 2 3; do echo line$i; sleep 0.2; done"):
            kinds.append(ev.kind)
            if ev.kind == "stdout":
                first = first or time.monotonic() - start
                out += ev.data
        assert kinds[0] == "started" and kinds[-1] == "exit"
        assert out == b"line1\nline2\nline3\n"
        assert first is not None and first < 0.45


def test_breaking_out_of_a_stream_kills_the_command(daemon):
    with Sandbox.create() as sb:
        for ev in sb.exec_stream("echo started; sleep 30"):
            if ev.kind == "stdout":
                break
        pid = daemon.last_pids[-1]
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                break
            if _is_zombie(pid):
                break
            time.sleep(0.05)
        else:
            pytest.fail(f"pid {pid} still running after the stream was abandoned")


def _is_zombie(pid: int) -> bool:
    try:
        with open(f"/proc/{pid}/stat") as f:
            return f.read().split()[2] == "Z"
    except OSError:
        return False


def test_files(daemon, tmp_path):
    with Sandbox.create() as sb:
        assert sb.fs.write("/tmp/a.txt", "hello\n") == 6
        assert sb.fs.read_text("/tmp/a.txt") == "hello\n"
        blob = os.urandom(700_000)
        sb.fs.write("/data/blob", blob)
        assert sb.fs.read("/data/blob") == blob
        local = tmp_path / "up.bin"
        local.write_bytes(blob[:300_001])
        assert sb.fs.upload(local, "/data/up.bin") == 300_001
        assert sb.fs.read("/data/up.bin") == blob[:300_001]
        with pytest.raises(NotFoundError):
            sb.fs.read("/missing")
        with pytest.raises(InvalidArgumentError):
            sb.fs.write("relative/path", b"x")


def test_fork(daemon):
    with Sandbox.create() as sb:
        kids = sb.fork(3)
        assert [k.parent for k in kids] == [sb.id] * 3
        assert len({k.id for k in kids}) == 3 and all(k.state == "running" for k in kids)
        assert sb.last_fork.parent_paused_ms == 4.5 and sb.last_fork.restore_ms == (30.0, 30.0, 30.0)
        assert sb.last_fork.layer == "layer-1" and sb.last_fork.dirty_bytes == 1 << 20
        with pytest.raises(InvalidArgumentError):
            sb.fork(0)
        for k in kids:
            k.destroy()


def test_expose(daemon):
    with Sandbox.create() as sb:
        assert sb.expose(8000) == ("127.0.0.1", 48000)
        assert sb.expose(8000, host_port=18080) == ("127.0.0.1", 18080)
        with pytest.raises(InvalidArgumentError):
            sb.expose(0)


def test_not_running(daemon):
    sb = Sandbox.create()
    daemon.sandboxes[sb.id].state = "creating"
    with pytest.raises(FailedPreconditionError):
        sb.exec("true")
    assert sb.refresh().state == "creating"


def test_public_api():
    for name in forkbox.__all__:
        assert hasattr(forkbox, name), name
