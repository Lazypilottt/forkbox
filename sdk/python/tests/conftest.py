"""A fake forkboxd for the SDK's unit tests: a real gRPC server on a Unix
socket, keeping sandboxes in memory and running commands as local processes."""

from __future__ import annotations

import os

# The fake runs commands with subprocess; keep gRPC's fork handlers (and
# their log lines on the child's stderr) out of it.
os.environ.setdefault("GRPC_ENABLE_FORK_SUPPORT", "0")
os.environ.setdefault("GRPC_VERBOSITY", "ERROR")

import hashlib
import shutil
import signal
import subprocess
import tempfile
import threading
import time
from concurrent import futures

import grpc
import pytest

from forkbox import Client, set_default_client
from forkbox._proto import forkbox_pb2 as pb
from forkbox._proto import forkbox_pb2_grpc as pbg


class FakeForkbox(pbg.ForkboxServicer):
    def __init__(self) -> None:
        self.sandboxes: dict[str, pb.Sandbox] = {}
        self.files: dict[tuple[str, str], bytes] = {}
        self.n = 0
        self.lock = threading.Lock()
        self.last_pids: list[int] = []

    def _get(self, sid: str, context) -> pb.Sandbox:
        sb = self.sandboxes.get(sid)
        if sb is None:
            context.abort(grpc.StatusCode.NOT_FOUND, f"sandbox not found: {sid}")
        if sb.state != "running":
            context.abort(grpc.StatusCode.FAILED_PRECONDITION, f"sandbox {sid} is {sb.state}")
        return sb

    def CreateSandbox(self, request, context):
        if not request.template:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "template is required")
        if request.template != "python":
            context.abort(grpc.StatusCode.NOT_FOUND, f"template not found: {request.template!r}")
        with self.lock:
            self.n += 1
            sid = f"sb-{self.n}"
        sb = pb.Sandbox(id=sid, template="python", template_id="tpl-x", state="running", pid=4000 + self.n,
                        created_at_unix_ms=int(time.time() * 1000), ttl_s=request.ttl_s)
        self.sandboxes[sid] = sb
        out = pb.Sandbox()
        out.CopyFrom(sb)
        out.timings.CopyFrom(pb.CreateTimings(total_ms=80.5, disk_ms=1, restore_ms=20, resume_ms=1, agent_ms=25, identity_ms=33))
        return out

    def GetSandbox(self, request, context):
        sb = self.sandboxes.get(request.id)
        if sb is None:
            context.abort(grpc.StatusCode.NOT_FOUND, f"sandbox not found: {request.id}")
        return sb

    def ListSandboxes(self, request, context):
        return pb.ListSandboxesResponse(sandboxes=list(self.sandboxes.values()))

    def DestroySandbox(self, request, context):
        if self.sandboxes.pop(request.id, None) is None:
            context.abort(grpc.StatusCode.NOT_FOUND, f"sandbox not found: {request.id}")
        return pb.DestroySandboxResponse()

    def Exec(self, request, context):
        self._get(request.sandbox_id, context)
        if not request.args:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "args is empty")
        env = dict(os.environ)
        env.update(e.split("=", 1) for e in request.env)
        start = time.monotonic()
        try:
            p = subprocess.Popen(list(request.args), stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                 env=env, cwd=request.cwd or None, start_new_session=True)
        except FileNotFoundError as e:
            context.abort(grpc.StatusCode.NOT_FOUND, str(e))
        self.last_pids.append(p.pid)
        context.add_callback(lambda: _kill(p))
        yield pb.ExecEvent(started=pb.ExecStarted(exec_id="e1", pid=p.pid))
        timed_out = False
        deadline = start + request.timeout_ms / 1000 if request.timeout_ms else None
        # Interleave stdout and stderr line by line, enough for the tests.
        os.set_blocking(p.stdout.fileno(), False)
        os.set_blocking(p.stderr.fileno(), False)
        while True:
            got = False
            for stream, kind in ((p.stdout, "stdout"), (p.stderr, "stderr")):
                data = stream.read(65536)
                if data:
                    got = True
                    yield pb.ExecEvent(**{kind: data})
            if p.poll() is not None and not got:
                rest_o, rest_e = p.stdout.read() or b"", p.stderr.read() or b""
                if rest_o:
                    yield pb.ExecEvent(stdout=rest_o)
                if rest_e:
                    yield pb.ExecEvent(stderr=rest_e)
                break
            if deadline and time.monotonic() > deadline and p.poll() is None:
                timed_out = True
                _kill(p)
            time.sleep(0.01)
        rc = p.returncode
        sig = ""
        if rc < 0:
            sig = {signal.SIGKILL: "killed", signal.SIGTERM: "terminated"}.get(-rc, str(-rc))
            rc = -1
        yield pb.ExecEvent(exit=pb.ExecExit(exit_code=rc, signal=sig, timed_out=timed_out,
                                            duration_ms=(time.monotonic() - start) * 1000))

    def ForkSandbox(self, request, context):
        parent = self._get(request.id, context)
        if not 0 < request.n <= 256:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "n must be 1 to 256")
        out = pb.ForkSandboxResponse(layer="layer-1")
        out.timings.CopyFrom(pb.ForkTimings(parent_paused_ms=4.5, merge_ms=12, total_ms=90, dirty_bytes=1 << 20))
        for _ in range(request.n):
            with self.lock:
                self.n += 1
                sid = f"sb-{self.n}"
            child = pb.Sandbox(id=sid, template=parent.template, template_id=parent.template_id, parent=parent.id,
                               state="running", pid=4000 + self.n, created_at_unix_ms=int(time.time() * 1000))
            self.sandboxes[sid] = child
            out.children.append(child)
            out.timings.restore_ms.append(30.0)
        return out

    def ExposePort(self, request, context):
        self._get(request.sandbox_id, context)
        if not 0 < request.guest_port < 65536:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "ports must be 1-65535")
        return pb.ExposePortResponse(host="127.0.0.1", host_port=request.host_port or 40000 + request.guest_port)

    def ReadFile(self, request, context):
        self._get(request.sandbox_id, context)
        data = self.files.get((request.sandbox_id, request.path))
        if data is None:
            context.abort(grpc.StatusCode.NOT_FOUND, f"open {request.path}: no such file or directory")
        for i in range(0, len(data), 100_000):
            yield pb.FileChunk(data=data[i : i + 100_000])
        yield pb.FileChunk(last=True, size=len(data), sha256=hashlib.sha256(data).digest())

    def WriteFile(self, request_iterator, context):
        first = next(request_iterator)
        if not first.HasField("header"):
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "write_file: the first message must be the header")
        h = first.header
        self._get(h.sandbox_id, context)
        if not h.path.startswith("/"):
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, f"path {h.path!r} must be absolute")
        buf = b"".join(m.data for m in request_iterator)
        self.files[(h.sandbox_id, h.path)] = buf
        return pb.WriteFileResponse(size=len(buf), sha256=hashlib.sha256(buf).digest())


def _kill(p: subprocess.Popen) -> None:
    if p.poll() is None:
        try:
            os.killpg(p.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


@pytest.fixture
def daemon():
    d = tempfile.mkdtemp(prefix="fbxpy", dir="/tmp")  # short: macOS caps socket paths
    sock = os.path.join(d, "forkboxd.sock")
    fake = FakeForkbox()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=16))
    pbg.add_ForkboxServicer_to_server(fake, server)
    server.add_insecure_port(f"unix://{sock}")
    server.start()
    client = Client(sock)
    set_default_client(client)
    yield fake
    set_default_client(None)
    client.close()
    server.stop(0)
    shutil.rmtree(d, ignore_errors=True)
