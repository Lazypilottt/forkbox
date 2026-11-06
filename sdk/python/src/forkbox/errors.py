"""Errors raised by the SDK, one class per forkboxd error code."""

from __future__ import annotations

import grpc


class ForkboxError(Exception):
    """Base class. ``code`` is the gRPC status code name, e.g. ``NOT_FOUND``."""

    code: str = "UNKNOWN"

    def __init__(self, message: str, code: str | None = None) -> None:
        super().__init__(message)
        if code:
            self.code = code


class NotFoundError(ForkboxError):
    code = "NOT_FOUND"


class InvalidArgumentError(ForkboxError):
    code = "INVALID_ARGUMENT"


class FailedPreconditionError(ForkboxError):
    code = "FAILED_PRECONDITION"


class AlreadyExistsError(ForkboxError):
    code = "ALREADY_EXISTS"


class ResourceExhaustedError(ForkboxError):
    code = "RESOURCE_EXHAUSTED"


class DeadlineExceededError(ForkboxError):
    code = "DEADLINE_EXCEEDED"


class UnavailableError(ForkboxError):
    code = "UNAVAILABLE"


class UnauthenticatedError(ForkboxError):
    code = "UNAUTHENTICATED"


class CommandError(ForkboxError):
    """Raised by ``exec(..., check=True)`` when the command fails."""

    code = "COMMAND_FAILED"

    def __init__(self, message: str, result: object) -> None:
        super().__init__(message)
        self.result = result


_BY_CODE: dict[grpc.StatusCode, type[ForkboxError]] = {
    grpc.StatusCode.NOT_FOUND: NotFoundError,
    grpc.StatusCode.INVALID_ARGUMENT: InvalidArgumentError,
    grpc.StatusCode.FAILED_PRECONDITION: FailedPreconditionError,
    grpc.StatusCode.ALREADY_EXISTS: AlreadyExistsError,
    grpc.StatusCode.RESOURCE_EXHAUSTED: ResourceExhaustedError,
    grpc.StatusCode.DEADLINE_EXCEEDED: DeadlineExceededError,
    grpc.StatusCode.UNAVAILABLE: UnavailableError,
    grpc.StatusCode.UNAUTHENTICATED: UnauthenticatedError,
}


def from_rpc(err: grpc.RpcError) -> ForkboxError:
    """Translate a gRPC error into the matching ForkboxError."""
    code = err.code() if hasattr(err, "code") else grpc.StatusCode.UNKNOWN
    details = err.details() if hasattr(err, "details") else str(err)
    cls = _BY_CODE.get(code, ForkboxError)
    return cls(details or str(code), code.name)
