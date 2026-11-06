#!/usr/bin/env python3
"""Create-to-first-exec latency and Firecracker memory per sandbox.

Run on the KVM host as root, with forkboxd running and the template built:

    sudo sdk/python/.venv/bin/python bench/restore.py --runs 100

Each run times Sandbox.create() plus a first exec of `true`, as the caller
sees it, and records the daemon's own per-step timings. Up to --keep
sandboxes stay alive at once, so the memory numbers include page sharing
between sandboxes of one template. Results go to bench/results/ as JSON and
a markdown table, with host details.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import platform
import statistics
import subprocess
import sys
import time
from collections import deque
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "sdk" / "python" / "src"))
from forkbox import Sandbox  # noqa: E402


def pct(xs: list[float], p: float) -> float:
    """Nearest-rank percentile."""
    s = sorted(xs)
    k = max(0, min(len(s) - 1, math.ceil(p / 100 * len(s)) - 1))
    return s[k]


def summary(xs: list[float]) -> dict[str, float]:
    return {
        "n": len(xs),
        "min": min(xs),
        "p50": pct(xs, 50),
        "p95": pct(xs, 95),
        "max": max(xs),
        "mean": statistics.fmean(xs),
        "stdev": statistics.stdev(xs) if len(xs) > 1 else 0.0,
    }


def smaps(pid: int) -> dict[str, int]:
    out: dict[str, int] = {}
    with open(f"/proc/{pid}/smaps_rollup") as f:
        for line in f:
            parts = line.split()
            if len(parts) == 3 and parts[2] == "kB":
                out[parts[0].rstrip(":")] = int(parts[1]) * 1024
    return out


def sh(*cmd: str) -> str:
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=10).stdout.strip()
    except (OSError, subprocess.SubprocessError):
        return ""


def host_details(data_dir: str) -> dict[str, object]:
    cpu = ""
    try:
        for line in open("/proc/cpuinfo"):
            if line.startswith("model name"):
                cpu = line.split(":", 1)[1].strip()
                break
    except OSError:
        pass
    mem = 0
    try:
        for line in open("/proc/meminfo"):
            if line.startswith("MemTotal:"):
                mem = int(line.split()[1]) * 1024
    except OSError:
        pass
    fc = sh(os.path.join(data_dir, "bin", "firecracker"), "--version").splitlines()
    return {
        "cpu": cpu,
        "cpus": os.cpu_count(),
        "ram_bytes": mem,
        "kernel": platform.release(),
        "virtualization": sh("systemd-detect-virt") or "unknown",
        "firecracker": fc[0] if fc else "",
        "data_dir_fs": sh("stat", "-f", "-c", "%T", data_dir),
        "python": platform.python_version(),
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--template", default="python")
    ap.add_argument("--runs", type=int, default=100)
    ap.add_argument("--keep", type=int, default=10, help="sandboxes kept alive at once")
    ap.add_argument("--data-dir", default=os.environ.get("FORKBOX_DATA_DIR", "/var/lib/forkbox"))
    ap.add_argument("--out", default=str(Path(__file__).resolve().parent / "results"))
    args = ap.parse_args()

    latency, server_total, mem_rss, mem_pss = [], [], [], []
    steps: dict[str, list[float]] = {k: [] for k in ("disk_ms", "restore_ms", "resume_ms", "agent_ms", "identity_ms")}
    alive: deque[Sandbox] = deque()
    started = datetime.now(timezone.utc)
    try:
        for i in range(args.runs):
            t0 = time.perf_counter()
            sb = Sandbox.create(args.template, ttl=600)
            r = sb.exec(["true"])
            dt = (time.perf_counter() - t0) * 1000
            if not r.ok:
                raise SystemExit(f"run {i}: first exec failed: {r}")
            alive.append(sb)
            latency.append(dt)
            server_total.append(sb.timings.total_ms)
            for k in steps:
                steps[k].append(getattr(sb.timings, k))
            m = smaps(sb.pid)
            mem_rss.append(m["Rss"] / 2**20)
            mem_pss.append(m["Pss"] / 2**20)
            while len(alive) > args.keep:
                alive.popleft().destroy()
            print(f"run {i + 1:3d}/{args.runs}: {dt:7.1f} ms  (daemon {sb.timings.total_ms:6.1f} ms, PSS {mem_pss[-1]:5.1f} MiB)", flush=True)
    finally:
        for sb in alive:
            sb.destroy()

    result = {
        "benchmark": "restore",
        "template": args.template,
        "runs": args.runs,
        "keep_alive": args.keep,
        "started_at": started.isoformat(),
        "host": host_details(args.data_dir),
        "create_to_first_exec_ms": summary(latency),
        "daemon_create_ms": summary(server_total),
        "daemon_steps_ms": {k: summary(v) for k, v in steps.items()},
        "firecracker_rss_mib": summary(mem_rss),
        "firecracker_pss_mib": summary(mem_pss),
        "raw": {"create_to_first_exec_ms": latency, "pss_mib": mem_pss},
    }
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    stamp = started.strftime("%Y%m%dT%H%M%SZ")
    (out / f"restore-{stamp}.json").write_text(json.dumps(result, indent=2) + "\n")
    md = table(result)
    (out / f"restore-{stamp}.md").write_text(md)
    print("\n" + md)
    return 0


def table(r: dict) -> str:
    h = r["host"]
    rows = [
        ("create to first exec (ms)", r["create_to_first_exec_ms"]),
        ("daemon create (ms)", r["daemon_create_ms"]),
        *((f"  {k.removesuffix('_ms')} (ms)", v) for k, v in r["daemon_steps_ms"].items()),
        ("Firecracker RSS (MiB)", r["firecracker_rss_mib"]),
        ("Firecracker PSS (MiB)", r["firecracker_pss_mib"]),
    ]
    lines = [
        f"### restore: template `{r['template']}`, {r['runs']} runs, up to {r['keep_alive']} alive",
        "",
        f"Host: {h['cpu']} ({h['cpus']} CPUs), {h['ram_bytes'] / 2**30:.1f} GiB RAM, kernel {h['kernel']}, "
        f"virtualization {h['virtualization']}, {h['firecracker']}, data dir on {h['data_dir_fs']}. {r['started_at']}.",
        "",
        "| metric | p50 | p95 | min | max | mean | stdev |",
        "| --- | ---: | ---: | ---: | ---: | ---: | ---: |",
    ]
    for name, s in rows:
        lines.append(f"| {name} | {s['p50']:.1f} | {s['p95']:.1f} | {s['min']:.1f} | {s['max']:.1f} | {s['mean']:.1f} | {s['stdev']:.1f} |")
    return "\n".join(lines) + "\n"


if __name__ == "__main__":
    sys.exit(main())
