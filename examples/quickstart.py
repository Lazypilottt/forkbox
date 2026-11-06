"""The README quick start. Run on the KVM host, as root, with forkboxd running
and the python template built."""

from forkbox import Sandbox

with Sandbox.create("python", ttl=600) as sb:
    print(f"sandbox {sb.id} ready in {sb.timings.total_ms:.0f} ms")

    r = sb.exec("python -c 'import numpy; print(numpy.arange(10).sum())'")
    print(r.exit_code, r.stdout.strip())  # 0 45

    for ev in sb.exec_stream("for i in 1 2 3; do echo tick $i; sleep 0.5; done"):
        if ev.kind == "stdout":
            print(ev.data.decode(), end="")

    sb.fs.write("/root/hello.py", "print('hello from the sandbox')\n")
    print(sb.exec("python /root/hello.py").stdout, end="")
# Leaving the block destroys the sandbox.
