"""The Forkbox MCP server: sandbox tools over stdio.

Every tool talks to forkboxd through the Python SDK (``$FORKBOX_SOCKET``, or
``$FORKBOX_ADDR`` with ``$FORKBOX_TOKEN``).
"""

from __future__ import annotations

import functools
from typing import Annotated

from mcp.server.mcpserver import MCPServer
from mcp.server.mcpserver.exceptions import ToolError
from mcp.types import ToolAnnotations
from pydantic import Field

from forkbox import Client, ForkboxError, Sandbox

# Output beyond these is cut, with a note on how to get the rest.
EXEC_OUTPUT_LIMIT = 16_000
READ_LIMIT = 64_000

INSTRUCTIONS = """Forkbox gives you isolated Linux sandboxes (Firecracker microVMs) that \
start in about a tenth of a second and can be forked: one running sandbox becomes \
N copies with the same files, processes and memory, each then independent. Use \
fork to try several approaches in parallel and keep the one that works, and \
checkpoint/rewind to undo. Sandboxes have outbound HTTP(S) and DNS. Destroy \
sandboxes you no longer need."""

server = MCPServer(name="forkbox", instructions=INSTRUCTIONS, version="0.2.0")
_client: Client | None = None


def tool(**kw):
    """server.tool, with Forkbox errors passed to the agent as tool errors.
    (Unexpected exceptions are hidden from clients by the MCP SDK.)"""

    def deco(fn):
        @functools.wraps(fn)
        def wrapper(*args, **kwargs):
            try:
                return fn(*args, **kwargs)
            except ForkboxError as e:
                raise ToolError(f"{e.code}: {e}") from None
            except ValueError as e:
                raise ToolError(f"INVALID_ARGUMENT: {e}") from None

        return server.tool(**kw)(wrapper)

    return deco


def client() -> Client:
    global _client
    if _client is None:
        _client = Client()
    return _client


def _sb(sandbox_id: str) -> Sandbox:
    return Sandbox.get(sandbox_id, client=client())


def _cut(data: bytes, limit: int, what: str, hint: str) -> str:
    text = data.decode("utf-8", errors="replace")
    if len(data) <= limit:
        return text
    head = data[: limit // 2].decode("utf-8", errors="replace")
    tail = data[-limit // 2 :].decode("utf-8", errors="replace")
    return (f"{head}\n\n[... {what} truncated: {len(data)} bytes in total, showing the first and last "
            f"{limit // 2} bytes. {hint} ...]\n\n{tail}")


def _describe(sb: Sandbox) -> str:
    line = f"{sb.id}: template {sb.template}, {sb.state}"
    if sb.parent:
        line += f", forked from {sb.parent}"
    if sb.ttl:
        line += f", expires {sb.ttl}s after creation"
    return line


@tool()
def create_sandbox(
    template: Annotated[str, Field(description="Template name, e.g. 'python' (Python 3.12, git, build tools, numpy, pytest) or 'node' (Node.js 22).")] = "python",
    ttl_seconds: Annotated[int, Field(ge=0, le=86400, description="Destroy the sandbox automatically after this many seconds; 0 keeps it until destroy is called.")] = 3600,
) -> str:
    """Create a new sandbox from a template. Returns its id, which every other tool takes."""
    sb = Sandbox.create(template, ttl=ttl_seconds or None, client=client())
    return f"Created sandbox {sb.id} from template {sb.template} in {sb.timings.total_ms:.0f} ms."


@tool(name="exec")
def exec_(
    sandbox_id: Annotated[str, Field(description="The sandbox to run in.")],
    command: Annotated[str, Field(description="A shell command, run with /bin/sh -c. The working directory is the image's default; use 'cd dir && ...' to change it.")],
    timeout_seconds: Annotated[float, Field(gt=0, le=3600, description="Kill the command (and its children) after this many seconds.")] = 120,
) -> str:
    """Run a shell command in a sandbox and return its exit code, stdout and stderr. Long output is truncated in the middle; redirect it to a file and use read_file to see all of it."""
    r = _sb(sandbox_id).exec(command, timeout=timeout_seconds)
    status = f"exit code {r.exit_code}"
    if r.timed_out:
        status = f"timed out after {timeout_seconds:g}s and was killed"
    elif r.signal:
        status = f"killed by signal {r.signal}"
    hint = "Redirect the output to a file (command > /tmp/out.txt) and read it with read_file and an offset."
    return (f"{status} ({r.duration_ms:.0f} ms)\n"
            f"--- stdout ---\n{_cut(r.stdout_bytes, EXEC_OUTPUT_LIMIT, 'stdout', hint)}\n"
            f"--- stderr ---\n{_cut(r.stderr_bytes, EXEC_OUTPUT_LIMIT, 'stderr', hint)}")


@tool()
def read_file(
    sandbox_id: Annotated[str, Field(description="The sandbox to read from.")],
    path: Annotated[str, Field(description="Absolute path in the sandbox.")],
    offset: Annotated[int, Field(ge=0, description="Byte offset to start at, for reading large files in parts.")] = 0,
    max_bytes: Annotated[int, Field(gt=0, le=READ_LIMIT, description=f"Most bytes to return (up to {READ_LIMIT}).")] = READ_LIMIT,
) -> str:
    """Read a text file from a sandbox. Large files are returned in parts: the answer says the next offset to read from."""
    data = _sb(sandbox_id).fs.read(path)
    part = data[offset : offset + max_bytes]
    text = part.decode("utf-8", errors="replace")
    end = offset + len(part)
    if end < len(data):
        text += f"\n\n[... {len(data) - end} more bytes of {len(data)}; call read_file again with offset={end} ...]"
    return text


@tool()
def write_file(
    sandbox_id: Annotated[str, Field(description="The sandbox to write to.")],
    path: Annotated[str, Field(description="Absolute path in the sandbox. Parent directories are created.")],
    content: Annotated[str, Field(description="The complete new content of the file (UTF-8).")],
) -> str:
    """Create or replace a file in a sandbox. The file appears only once it is completely written."""
    n = _sb(sandbox_id).fs.write(path, content)
    return f"Wrote {n} bytes to {path}."


@tool()
def fork(
    sandbox_id: Annotated[str, Field(description="The running sandbox to copy.")],
    n: Annotated[int, Field(ge=1, le=50, description="How many copies to make.")],
) -> str:
    """Fork a running sandbox into n copies. Each copy has the same files, running processes and memory at this moment, then runs independently (changes in one are invisible to the others and to the original). The original keeps running. Use it to try different approaches in parallel."""
    parent = _sb(sandbox_id)
    kids = parent.fork(n)
    t = parent.last_fork
    ids = "\n".join(k.id for k in kids)
    return (f"Forked {sandbox_id} into {len(kids)} sandboxes in {t.total_ms:.0f} ms "
            f"(the original was paused for {t.parent_paused_ms:.1f} ms):\n{ids}")


@tool()
def checkpoint(
    sandbox_id: Annotated[str, Field(description="The sandbox to save.")],
    name: Annotated[str, Field(pattern=r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", description="A name for this point, unique within the sandbox, e.g. 'before-refactor'.")],
) -> str:
    """Save a sandbox's files, running processes and memory under a name, to rewind to later. The sandbox keeps running."""
    cp = _sb(sandbox_id).checkpoint(name)
    return f"Checkpoint '{cp.name}' of {sandbox_id} saved (paused {cp.paused_ms:.1f} ms)."


@tool(annotations=ToolAnnotations(destructiveHint=True))
def rewind(
    sandbox_id: Annotated[str, Field(description="The sandbox to roll back.")],
    name: Annotated[str, Field(description="The checkpoint to return to.")],
) -> str:
    """Roll a sandbox back to a checkpoint: files, running processes and memory become what they were then; everything since is lost. The sandbox id stays the same."""
    _sb(sandbox_id).rewind(name)
    return f"Sandbox {sandbox_id} is back at checkpoint '{name}'."


@tool(annotations=ToolAnnotations(readOnlyHint=True))
def list_sandboxes() -> str:
    """List the sandboxes that exist, with their template, state and parent."""
    sbs = Sandbox.list(client=client())
    if not sbs:
        return "No sandboxes."
    return "\n".join(_describe(sb) for sb in sbs)


@tool(annotations=ToolAnnotations(destructiveHint=True))
def destroy(sandbox_id: Annotated[str, Field(description="The sandbox to destroy.")]) -> str:
    """Destroy a sandbox and everything in it. Its checkpoints are kept."""
    _sb(sandbox_id).destroy()
    return f"Destroyed {sandbox_id}."


def main() -> None:
    server.run("stdio")


if __name__ == "__main__":
    main()
