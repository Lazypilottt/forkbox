from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class Sandbox(_message.Message):
    __slots__ = ("id", "template", "template_id", "parent", "state", "created_at_unix_ms", "ttl_s", "pid", "timings")
    ID_FIELD_NUMBER: _ClassVar[int]
    TEMPLATE_FIELD_NUMBER: _ClassVar[int]
    TEMPLATE_ID_FIELD_NUMBER: _ClassVar[int]
    PARENT_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    TTL_S_FIELD_NUMBER: _ClassVar[int]
    PID_FIELD_NUMBER: _ClassVar[int]
    TIMINGS_FIELD_NUMBER: _ClassVar[int]
    id: str
    template: str
    template_id: str
    parent: str
    state: str
    created_at_unix_ms: int
    ttl_s: int
    pid: int
    timings: CreateTimings
    def __init__(self, id: _Optional[str] = ..., template: _Optional[str] = ..., template_id: _Optional[str] = ..., parent: _Optional[str] = ..., state: _Optional[str] = ..., created_at_unix_ms: _Optional[int] = ..., ttl_s: _Optional[int] = ..., pid: _Optional[int] = ..., timings: _Optional[_Union[CreateTimings, _Mapping]] = ...) -> None: ...

class CreateTimings(_message.Message):
    __slots__ = ("total_ms", "disk_ms", "restore_ms", "resume_ms", "agent_ms", "identity_ms")
    TOTAL_MS_FIELD_NUMBER: _ClassVar[int]
    DISK_MS_FIELD_NUMBER: _ClassVar[int]
    RESTORE_MS_FIELD_NUMBER: _ClassVar[int]
    RESUME_MS_FIELD_NUMBER: _ClassVar[int]
    AGENT_MS_FIELD_NUMBER: _ClassVar[int]
    IDENTITY_MS_FIELD_NUMBER: _ClassVar[int]
    total_ms: float
    disk_ms: float
    restore_ms: float
    resume_ms: float
    agent_ms: float
    identity_ms: float
    def __init__(self, total_ms: _Optional[float] = ..., disk_ms: _Optional[float] = ..., restore_ms: _Optional[float] = ..., resume_ms: _Optional[float] = ..., agent_ms: _Optional[float] = ..., identity_ms: _Optional[float] = ...) -> None: ...

class CreateSandboxRequest(_message.Message):
    __slots__ = ("template", "ttl_s")
    TEMPLATE_FIELD_NUMBER: _ClassVar[int]
    TTL_S_FIELD_NUMBER: _ClassVar[int]
    template: str
    ttl_s: int
    def __init__(self, template: _Optional[str] = ..., ttl_s: _Optional[int] = ...) -> None: ...

class GetSandboxRequest(_message.Message):
    __slots__ = ("id",)
    ID_FIELD_NUMBER: _ClassVar[int]
    id: str
    def __init__(self, id: _Optional[str] = ...) -> None: ...

class ListSandboxesRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ListSandboxesResponse(_message.Message):
    __slots__ = ("sandboxes",)
    SANDBOXES_FIELD_NUMBER: _ClassVar[int]
    sandboxes: _containers.RepeatedCompositeFieldContainer[Sandbox]
    def __init__(self, sandboxes: _Optional[_Iterable[_Union[Sandbox, _Mapping]]] = ...) -> None: ...

class ExecRequest(_message.Message):
    __slots__ = ("sandbox_id", "args", "env", "cwd", "timeout_ms")
    SANDBOX_ID_FIELD_NUMBER: _ClassVar[int]
    ARGS_FIELD_NUMBER: _ClassVar[int]
    ENV_FIELD_NUMBER: _ClassVar[int]
    CWD_FIELD_NUMBER: _ClassVar[int]
    TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    sandbox_id: str
    args: _containers.RepeatedScalarFieldContainer[str]
    env: _containers.RepeatedScalarFieldContainer[str]
    cwd: str
    timeout_ms: int
    def __init__(self, sandbox_id: _Optional[str] = ..., args: _Optional[_Iterable[str]] = ..., env: _Optional[_Iterable[str]] = ..., cwd: _Optional[str] = ..., timeout_ms: _Optional[int] = ...) -> None: ...

class ExecEvent(_message.Message):
    __slots__ = ("started", "stdout", "stderr", "exit")
    STARTED_FIELD_NUMBER: _ClassVar[int]
    STDOUT_FIELD_NUMBER: _ClassVar[int]
    STDERR_FIELD_NUMBER: _ClassVar[int]
    EXIT_FIELD_NUMBER: _ClassVar[int]
    started: ExecStarted
    stdout: bytes
    stderr: bytes
    exit: ExecExit
    def __init__(self, started: _Optional[_Union[ExecStarted, _Mapping]] = ..., stdout: _Optional[bytes] = ..., stderr: _Optional[bytes] = ..., exit: _Optional[_Union[ExecExit, _Mapping]] = ...) -> None: ...

class ExecStarted(_message.Message):
    __slots__ = ("exec_id", "pid")
    EXEC_ID_FIELD_NUMBER: _ClassVar[int]
    PID_FIELD_NUMBER: _ClassVar[int]
    exec_id: str
    pid: int
    def __init__(self, exec_id: _Optional[str] = ..., pid: _Optional[int] = ...) -> None: ...

class ExecExit(_message.Message):
    __slots__ = ("exit_code", "signal", "timed_out", "duration_ms")
    EXIT_CODE_FIELD_NUMBER: _ClassVar[int]
    SIGNAL_FIELD_NUMBER: _ClassVar[int]
    TIMED_OUT_FIELD_NUMBER: _ClassVar[int]
    DURATION_MS_FIELD_NUMBER: _ClassVar[int]
    exit_code: int
    signal: str
    timed_out: bool
    duration_ms: float
    def __init__(self, exit_code: _Optional[int] = ..., signal: _Optional[str] = ..., timed_out: _Optional[bool] = ..., duration_ms: _Optional[float] = ...) -> None: ...

class ReadFileRequest(_message.Message):
    __slots__ = ("sandbox_id", "path")
    SANDBOX_ID_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    sandbox_id: str
    path: str
    def __init__(self, sandbox_id: _Optional[str] = ..., path: _Optional[str] = ...) -> None: ...

class FileChunk(_message.Message):
    __slots__ = ("data", "last", "size", "sha256")
    DATA_FIELD_NUMBER: _ClassVar[int]
    LAST_FIELD_NUMBER: _ClassVar[int]
    SIZE_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    data: bytes
    last: bool
    size: int
    sha256: bytes
    def __init__(self, data: _Optional[bytes] = ..., last: _Optional[bool] = ..., size: _Optional[int] = ..., sha256: _Optional[bytes] = ...) -> None: ...

class WriteFileHeader(_message.Message):
    __slots__ = ("sandbox_id", "path", "mode", "make_parents")
    SANDBOX_ID_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    MODE_FIELD_NUMBER: _ClassVar[int]
    MAKE_PARENTS_FIELD_NUMBER: _ClassVar[int]
    sandbox_id: str
    path: str
    mode: int
    make_parents: bool
    def __init__(self, sandbox_id: _Optional[str] = ..., path: _Optional[str] = ..., mode: _Optional[int] = ..., make_parents: _Optional[bool] = ...) -> None: ...

class WriteFileRequest(_message.Message):
    __slots__ = ("header", "data")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    header: WriteFileHeader
    data: bytes
    def __init__(self, header: _Optional[_Union[WriteFileHeader, _Mapping]] = ..., data: _Optional[bytes] = ...) -> None: ...

class WriteFileResponse(_message.Message):
    __slots__ = ("size", "sha256")
    SIZE_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    size: int
    sha256: bytes
    def __init__(self, size: _Optional[int] = ..., sha256: _Optional[bytes] = ...) -> None: ...

class DestroySandboxRequest(_message.Message):
    __slots__ = ("id",)
    ID_FIELD_NUMBER: _ClassVar[int]
    id: str
    def __init__(self, id: _Optional[str] = ...) -> None: ...

class DestroySandboxResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...
