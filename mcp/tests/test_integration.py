"""The MCP tools against a real forkboxd (FORKBOX_INTEGRATION=1, as root)."""

import asyncio
import os
import re

import pytest

import forkbox_mcp.server as srv

pytestmark = [
    pytest.mark.integration,
    pytest.mark.skipif(os.environ.get("FORKBOX_INTEGRATION") != "1", reason="needs a running forkboxd"),
]


def call(_tool, **args):
    r = asyncio.run(srv.server.call_tool(_tool, args))
    return "".join(getattr(c, "text", "") for c in r.content)


def test_agent_session():
    srv._client = None
    out = call("create_sandbox", template="python", ttl_seconds=600)
    sid = re.search(r"sandbox (sb-[0-9a-f]+)", out).group(1)
    try:
        call("write_file", sandbox_id=sid, path="/root/app.py", content="print(sum(range(10)))\n")
        assert "45" in call("exec", sandbox_id=sid, command="python /root/app.py")
        out = call("fork", sandbox_id=sid, n=2)
        kids = re.findall(r"^(sb-[0-9a-f]+)$", out, re.M)
        assert len(kids) == 2
        for k in kids:
            assert "45" in call("exec", sandbox_id=k, command="python /root/app.py")
            call("destroy", sandbox_id=k)
        call("checkpoint", sandbox_id=sid, name="good")
        call("exec", sandbox_id=sid, command="rm /root/app.py")
        call("rewind", sandbox_id=sid, name="good")
        assert "45" in call("exec", sandbox_id=sid, command="python /root/app.py")
    finally:
        call("destroy", sandbox_id=sid)
