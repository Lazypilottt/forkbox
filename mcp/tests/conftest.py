"""Run the MCP tools against the SDK's fake daemon (sdk/python/tests)."""

import os
import shutil
import sys
import tempfile
from concurrent import futures
from pathlib import Path

import importlib.util  # noqa: E402

import grpc  # noqa: E402
import pytest  # noqa: E402

# The SDK's fake daemon, loaded under its own name (both test directories
# have a conftest.py).
_spec = importlib.util.spec_from_file_location(
    "forkbox_sdk_fake", Path(__file__).resolve().parents[2] / "sdk" / "python" / "tests" / "conftest.py")
_fake = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_fake)
FakeForkbox = _fake.FakeForkbox

import forkbox_mcp.server as srv  # noqa: E402
from forkbox import Client  # noqa: E402
from forkbox._proto import forkbox_pb2_grpc as pbg  # noqa: E402


@pytest.fixture
def daemon():
    d = tempfile.mkdtemp(prefix="fbxmcp", dir="/tmp")
    sock = os.path.join(d, "forkboxd.sock")
    fake = FakeForkbox()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=16))
    pbg.add_ForkboxServicer_to_server(fake, server)
    server.add_insecure_port(f"unix://{sock}")
    server.start()
    srv._client = Client(sock)
    yield fake
    srv._client.close()
    srv._client = None
    server.stop(0)
    shutil.rmtree(d, ignore_errors=True)
