"""Clone hooks for Python processes in Forkbox sandboxes.

A process that was running when a sandbox was forked or checkpointed
continues in every child with the same in-memory state, including the state
of user-space random number generators. The kernel's RNG is reseeded on
every restore, but these are not:

- ``random`` (the global Mersenne Twister) and ``numpy.random``'s legacy
  global generator are reseeded here from the kernel;
- OpenSSL's DRBG (``ssl.RAND_bytes``) gets fresh kernel entropy here;
- generators you created yourself, such as ``numpy.random.default_rng()``,
  cannot be found from outside: register a callback with :func:`on_restore`
  that re-creates them.

``uuid.uuid4``, ``os.urandom``, ``secrets`` and ``random.SystemRandom`` read
the kernel and need nothing.

The guest agent writes a new token to /run/forkbox/generation after every
restore; a daemon thread in each Python process watches it. sitecustomize
starts it automatically in Forkbox templates (set FORKBOX_CLONE_HOOKS=0 to
opt out).
"""

from __future__ import annotations

import os
import random
import sys
import threading
import time
from collections.abc import Callable

GENERATION_FILE = "/run/forkbox/generation"

_callbacks: list[Callable[[], None]] = []
_lock = threading.Lock()
_started = False
_restores = 0


def on_restore(fn: Callable[[], None]) -> Callable[[], None]:
    """Call ``fn`` (in the hook thread) after each restore of this sandbox."""
    with _lock:
        _callbacks.append(fn)
    return fn


def restores() -> int:
    """How many restores this process has seen (0 before the first fork)."""
    return _restores


def generation() -> str | None:
    try:
        with open(GENERATION_FILE) as f:
            return f.read()
    except OSError:
        return None


def reseed() -> None:
    """Reseed the generators this module knows about, then run callbacks."""
    global _restores
    random.seed()  # from os.urandom
    np = sys.modules.get("numpy")
    if np is not None:
        try:
            np.random.seed()
        except Exception:
            pass
    ssl = sys.modules.get("ssl")
    if ssl is not None:
        try:
            ssl.RAND_add(os.urandom(64), 64.0)
        except Exception:
            pass
    with _lock:
        cbs = list(_callbacks)
    _restores += 1
    for cb in cbs:
        try:
            cb()
        except Exception as e:  # a broken callback must not stop the others
            print(f"forkbox_guest: on_restore callback {cb!r} failed: {e}", file=sys.stderr)


def install(interval: float = 0.05) -> None:
    """Start watching for restores (idempotent)."""
    global _started
    with _lock:
        if _started:
            return
        _started = True
    last = generation()

    def loop() -> None:
        nonlocal last
        while True:
            time.sleep(interval)
            cur = generation()
            if cur is not None and cur != last:
                last = cur
                reseed()

    threading.Thread(target=loop, name="forkbox-clone-hook", daemon=True).start()
