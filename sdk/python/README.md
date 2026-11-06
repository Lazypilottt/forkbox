# forkbox (Python SDK)

```python
from forkbox import Sandbox

with Sandbox.create("python", ttl=600) as sb:
    r = sb.exec("python -c 'import numpy; print(numpy.__version__)'")
    print(r.exit_code, r.stdout)
    for ev in sb.exec_stream("for i in 1 2 3; do echo $i; sleep 1; done"):
        if ev.kind == "stdout":
            print(ev.data.decode(), end="")
    sb.fs.write("/tmp/hello.txt", "hi\n")
    print(sb.fs.read_text("/tmp/hello.txt"))
```

The client talks to `forkboxd` on its Unix socket (`$FORKBOX_SOCKET`, default
`/var/lib/forkbox/run/forkboxd.sock`, owner-only), or over TCP with
`$FORKBOX_ADDR` and `$FORKBOX_TOKEN`.

Development:

```sh
uv venv --python 3.12 .venv && uv pip install --python .venv/bin/python -e '.[dev]'
.venv/bin/pytest                                   # unit tests, against an in-process fake daemon
FORKBOX_INTEGRATION=1 .venv/bin/pytest -m integration   # against a real forkboxd (KVM host, as root)
```

`make proto` at the repository root regenerates `src/forkbox/_proto`.
