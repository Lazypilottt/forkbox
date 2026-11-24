"""race, map, pool stats and the async API, against the fake daemon."""

import asyncio
import time

import pytest

import forkbox
from forkbox import Sandbox, pool_stats
from forkbox.aio import AsyncClient, AsyncSandbox


def test_race_returns_the_one_that_passes(daemon):
    with Sandbox.create() as sb:
        def run(child, i):
            child.fs.write("/attempt", str(i))
            time.sleep(0.01 * (i % 5))
            return i * 10

        def check(child):
            return child.fs.read_text("/attempt") == "13"

        r = sb.race(20, run, check)
        assert r.index == 13 and r.winner is not None and r.winner.id in daemon.sandboxes
        assert len(r.attempts) == 20 and r.attempts[13].passed and r.attempts[13].result == 130
        # Only the parent and the winner are left.
        assert set(daemon.sandboxes) == {sb.id, r.winner.id}
        assert all(a.cancelled or not a.passed for i, a in enumerate(r.attempts) if i != 13)
        r.winner.destroy()


def test_race_without_a_winner_destroys_everyone(daemon):
    with Sandbox.create() as sb:
        r = sb.race(4, lambda c, i: i, lambda c: False)
        assert r.winner is None and r.index is None and set(daemon.sandboxes) == {sb.id}
        assert [a.passed for a in r.attempts] == [False] * 4
        with pytest.raises(ValueError):
            sb.race(2, lambda c, i: i, None)


def test_race_timeout_and_errors(daemon):
    with Sandbox.create() as sb:
        def run(child, i):
            if i == 0:
                raise RuntimeError("boom")
            time.sleep(5)

        t = time.monotonic()
        r = sb.race(3, run, lambda c: True, timeout=0.5)
        assert r.winner is None and time.monotonic() - t < 8
        assert r.attempts[0].error == "RuntimeError: boom" and not r.attempts[0].passed
        assert r.attempts[1].cancelled and r.attempts[2].cancelled
        assert set(daemon.sandboxes) == {sb.id}


def test_map_keeps_every_result(daemon):
    with Sandbox.create() as sb:
        attempts = sb.map(5, lambda c, i: i * i)
        assert [a.result for a in attempts] == [0, 1, 4, 9, 16]
        assert set(daemon.sandboxes) == {sb.id}
        kept = sb.map(2, lambda c, i: c.id, keep=True)
        assert {a.result for a in kept} <= set(daemon.sandboxes)


def test_pool_stats(daemon):
    (p,) = pool_stats()
    assert p["template"] == "python" and p["hits"] == 5 and p["demand_10m"] == 6


def test_async_api(daemon):
    sock = forkbox.default_client().target.removeprefix("unix://")

    async def main():
        async with AsyncClient(sock) as c:
            async with await AsyncSandbox.create("python", client=c) as sb:
                r = await sb.exec("echo hi; echo err >&2; exit 2")
                assert (r.stdout, r.stderr, r.exit_code) == ("hi\n", "err\n", 2)
                lines = [ev.data async for ev in sb.exec_stream(["sh", "-c", "echo a; echo b"]) if ev.kind == "stdout"]
                assert b"".join(lines) == b"a\nb\n"
                await sb.fs.write("/x", "async")
                assert await sb.fs.read_text("/x") == "async"
                kids = await sb.fork(2)
                assert len(kids) == 2 and sb.last_fork is not None

                async def run(child, i):
                    await child.fs.write("/n", str(i))

                async def check(child):
                    return await child.fs.read_text("/n") == "3"

                rr = await sb.race(6, run, check)
                assert rr.index == 3 and rr.winner is not None
                await rr.winner.destroy()
                for k in kids:
                    await k.destroy()
            assert sb.id not in daemon.sandboxes
            assert [s.id for s in await AsyncSandbox.list(client=c)] == []

    asyncio.run(main())
