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
from .sandbox import (
    Attempt, Checkpoint, CreateTimings, ExecEvent, ExecResult, FileSystem, ForkTimings, Limits, RaceResult, Sandbox, pool_stats,
)

__version__ = "0.1.0"

__all__ = [
    "AlreadyExistsError",
    "Attempt",
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
    "Limits",
    "NotFoundError",
    "RaceResult",
    "ResourceExhaustedError",
    "Sandbox",
    "UnauthenticatedError",
    "UnavailableError",
    "default_client",
    "pool_stats",
    "set_default_client",
]
