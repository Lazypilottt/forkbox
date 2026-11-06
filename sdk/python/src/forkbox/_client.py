"""Connection to forkboxd."""

from __future__ import annotations

import os
import threading

import grpc

from ._proto import forkbox_pb2_grpc

DEFAULT_SOCKET = "/var/lib/forkbox/run/forkboxd.sock"


class Client:
    """A connection to forkboxd.

    By default it uses the daemon's Unix socket: ``$FORKBOX_SOCKET``, else
    ``/var/lib/forkbox/run/forkboxd.sock`` (owner-only, so run as the
    daemon's user). For the optional TCP listener, set ``$FORKBOX_ADDR``
    (``host:port``) and ``$FORKBOX_TOKEN``, or pass ``address`` and ``token``.
    """

    def __init__(
        self,
        socket: str | None = None,
        *,
        address: str | None = None,
        token: str | None = None,
    ) -> None:
        address = address or os.environ.get("FORKBOX_ADDR")
        token = token or os.environ.get("FORKBOX_TOKEN")
        options = [
            ("grpc.max_receive_message_length", 8 << 20),
            ("grpc.max_send_message_length", 8 << 20),
        ]
        if address:
            self.target = address
            self._channel = grpc.insecure_channel(address, options=options)
            self._metadata: tuple[tuple[str, str], ...] = (
                (("authorization", f"Bearer {token}"),) if token else ()
            )
        else:
            path = socket or os.environ.get("FORKBOX_SOCKET") or DEFAULT_SOCKET
            self.target = f"unix://{path}"
            self._channel = grpc.insecure_channel(self.target, options=options)
            self._metadata = ()
        self.stub = forkbox_pb2_grpc.ForkboxStub(self._channel)

    @property
    def metadata(self) -> tuple[tuple[str, str], ...]:
        return self._metadata

    def close(self) -> None:
        self._channel.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


_default: Client | None = None
_default_lock = threading.Lock()


def default_client() -> Client:
    """The shared client used when none is passed explicitly."""
    global _default
    with _default_lock:
        if _default is None:
            _default = Client()
        return _default


def set_default_client(client: Client | None) -> None:
    """Replace the shared client (for tests, or to point at another daemon)."""
    global _default
    with _default_lock:
        _default = client
