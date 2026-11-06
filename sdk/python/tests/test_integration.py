"""Against a real forkboxd on a KVM host: FORKBOX_INTEGRATION=1, as root, with
the python template built."""

from __future__ import annotations

import hashlib
import os
import time
from concurrent.futures import ThreadPoolExecutor

import pytest

from forkbox import Client, NotFoundError, Sandbox, set_default_client

pytestmark = [
    pytest.mark.integration,
    pytest.mark.skipif(os.environ.get("FORKBOX_INTEGRATION") != "1", reason="needs a running forkboxd (FORKBOX_INTEGRATION=1)"),
]


@pytest.fixture(autouse=True)
def real_daemon():
    c = Client()
    set_default_client(c)
    yield
    set_default_client(None)
    c.close()


def test_end_to_end():
    with Sandbox.create("python", ttl=300) as sb:
        assert sb.state == "running" and sb.pid > 0 and sb.timings.total_ms > 0
        r = sb.exec("python -c 'import numpy, socket; print(numpy.ones(3).sum(), socket.gethostname())'")
        assert r.ok, r.stderr
        assert r.stdout.split() == ["3.0", sb.id]

        lines = [ev.data for ev in sb.exec_stream("for i in 1 2 3; do echo $i; sleep 0.1; done") if ev.kind == "stdout"]
        assert b"".join(lines) == b"1\n2\n3\n"

        blob = os.urandom(5 << 20)
        sb.fs.write("/root/blob.bin", blob)
        assert sb.fs.read("/root/blob.bin") == blob
        r = sb.exec("sha256sum /root/blob.bin")
        assert r.stdout.split()[0] == hashlib.sha256(blob).hexdigest()

        r = sb.exec("sleep 30", timeout=0.5)
        assert r.timed_out
        assert Sandbox.get(sb.id).id in [s.id for s in Sandbox.list()]
    with pytest.raises(NotFoundError):
        Sandbox.get(sb.id)


def test_unknown_template():
    with pytest.raises(NotFoundError):
        Sandbox.create("no-such-template")


def test_parallel_creates_are_distinct():
    with ThreadPoolExecutor(5) as pool:
        sbs = list(pool.map(lambda _: Sandbox.create("python", ttl=300), range(5)))
    try:
        ids = {sb.exec("cat /etc/machine-id").stdout.strip() for sb in sbs}
        assert len(ids) == 5
    finally:
        for sb in sbs:
            sb.destroy()


def test_ttl_expires():
    sb = Sandbox.create("python", ttl=1)
    deadline = time.monotonic() + 40  # the daemon sweeps every 10 s
    while time.monotonic() < deadline:
        try:
            Sandbox.get(sb.id)
        except NotFoundError:
            return
        time.sleep(1)
    sb.destroy()
    pytest.fail("sandbox outlived its TTL")


def test_fork():
    with Sandbox.create("python", ttl=300) as sb:
        sb.exec("echo from-parent > /root/state.txt; python -c 'open(\"/root/x\", \"w\").write(\"1\")'", check=True)
        kids = sb.fork(3)
        try:
            assert len(kids) == 3 and {k.parent for k in kids} == {sb.id}
            assert sb.last_fork.parent_paused_ms > 0 and len(sb.last_fork.restore_ms) == 3
            for k in kids:
                assert k.fs.read_text("/root/state.txt") == "from-parent\n"
                assert k.exec("hostname").stdout.strip() == k.id
            kids[0].fs.write("/root/state.txt", "changed in child 0\n")
            assert kids[1].fs.read_text("/root/state.txt") == "from-parent\n"
            assert sb.fs.read_text("/root/state.txt") == "from-parent\n"
        finally:
            for k in kids:
                k.destroy()
