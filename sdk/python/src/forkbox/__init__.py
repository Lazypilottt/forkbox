"""Forkbox: Firecracker sandboxes that fork in milliseconds.

    from forkbox import Sandbox

    with Sandbox.create("python") as sb:
        print(sb.exec("python -c 'print(6 * 7)'").stdout)
"""

from ._client import Client, default_client, set_default_client
from .errors import (
    AlreadyExistsError,
    CommandError,
    DeadlineExceededError,
    FailedPreconditionError,
    ForkboxError,
    InvalidArgumentError,
    NotFoundError,
    ResourceExhaustedError,
    UnauthenticatedError,
    UnavailableError,
)
from .sandbox import Checkpoint, CreateTimings, ExecEvent, ExecResult, FileSystem, ForkTimings, Sandbox

__version__ = "0.1.0"

__all__ = [
    "AlreadyExistsError",
    "Checkpoint",
    "Client",
    "CommandError",
    "CreateTimings",
    "DeadlineExceededError",
    "ExecEvent",
    "ExecResult",
    "FailedPreconditionError",
    "FileSystem",
    "ForkTimings",
    "ForkboxError",
    "InvalidArgumentError",
    "NotFoundError",
    "ResourceExhaustedError",
    "Sandbox",
    "UnauthenticatedError",
    "UnavailableError",
    "default_client",
    "set_default_client",
]
