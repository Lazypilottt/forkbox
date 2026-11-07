"""The MCP tools, called the way an MCP client calls them."""

import asyncio
import re

from mcp.server.mcpserver.exceptions import ToolError

from forkbox_mcp import server as srv


def call(_tool, **args):
    """(is_error, text), as an MCP client would see the result."""
    try:
        r = asyncio.run(srv.server.call_tool(_tool, args))
    except ToolError as e:
        return True, str(e)
    text = "".join(getattr(c, "text", "") for c in r.content)
    return r.is_error, text


def test_tools_are_listed_with_schemas():
    tools = {t.name: t for t in asyncio.run(srv.server.list_tools())}
    assert set(tools) == {"create_sandbox", "exec", "read_file", "write_file", "fork", "checkpoint", "rewind", "list_sandboxes", "destroy"}
    for t in tools.values():
        assert t.description and len(t.description) > 30, t.name
        for name, prop in t.input_schema.get("properties", {}).items():
            assert prop.get("description"), f"{t.name}.{name} has no description"
    assert tools["destroy"].annotations.destructive_hint and tools["rewind"].annotations.destructive_hint
    assert tools["list_sandboxes"].annotations.read_only_hint


def test_create_exec_files_destroy(daemon):
    err, out = call("create_sandbox", template="python")
    assert not err, out
    sid = re.search(r"sandbox (sb-\d+)", out).group(1)
    err, out = call("exec", sandbox_id=sid, command="echo hello; echo oops >&2; exit 3")
    assert not err and "exit code 3" in out and "hello" in out and "oops" in out
    err, out = call("write_file", sandbox_id=sid, path="/tmp/a.txt", content="line\n" * 3)
    assert not err and "Wrote 15 bytes" in out
    assert call("read_file", sandbox_id=sid, path="/tmp/a.txt") == (False, "line\nline\nline\n")
    err, out = call("list_sandboxes")
    assert sid in out and "python" in out
    err, out = call("destroy", sandbox_id=sid)
    assert not err and sid not in daemon.sandboxes


def test_long_output_is_cut_with_a_hint(daemon):
    _, out = call("create_sandbox")
    sid = re.search(r"(sb-\d+)", out).group(1)
    err, out = call("exec", sandbox_id=sid, command="yes x | head -c 100000")
    assert not err and "stdout truncated: 100000 bytes" in out and "read_file" in out
    assert len(out) < 20_000


def test_read_file_in_parts(daemon):
    _, out = call("create_sandbox")
    sid = re.search(r"(sb-\d+)", out).group(1)
    call("write_file", sandbox_id=sid, path="/tmp/big.txt", content="abcdefghij" * 10_000)
    _, part = call("read_file", sandbox_id=sid, path="/tmp/big.txt", max_bytes=30_000)
    assert part.startswith("abcdefghij") and "call read_file again with offset=30000" in part
    _, rest = call("read_file", sandbox_id=sid, path="/tmp/big.txt", offset=90_000)
    assert rest == "abcdefghij" * 1000


def test_fork_checkpoint_rewind(daemon):
    _, out = call("create_sandbox")
    sid = re.search(r"(sb-\d+)", out).group(1)
    err, out = call("fork", sandbox_id=sid, n=3)
    assert not err and "into 3 sandboxes" in out and len(re.findall(r"^sb-\d+$", out, re.M)) == 3
    err, out = call("checkpoint", sandbox_id=sid, name="before")
    assert not err and "'before'" in out
    err, out = call("rewind", sandbox_id=sid, name="before")
    assert not err and "back at checkpoint 'before'" in out


def test_errors_come_back_as_tool_errors(daemon):
    err, out = call("exec", sandbox_id="sb-404", command="true")
    assert err and "NOT_FOUND" in out and "sb-404" in out
    err, out = call("fork", sandbox_id="sb-404", n=0)
    assert err
