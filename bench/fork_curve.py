#!/usr/bin/env python3
"""Fork latency against dirty memory, with baselines.

Run on the KVM host as root, with forkboxd running:

    sudo sdk/python/.venv/bin/python bench/fork_curve.py

For each amount of dirty memory (a guest program allocates and writes it
just before the fork) and each N, a fresh parent is forked into N children.
It records the daemon's timings (pause, diff snapshot, merge, per-child
restore, total), the dirty bytes actually merged, and the PSS of every
child right after the fork. Baselines on the same host: a cold boot of the
template, a full-snapshot restore from a byte copy (no copy-on-write), and
`docker run` of the template's base image. Writes JSON, a PNG chart and a
markdown table to bench/results/.
"""

from __future__ import annotations

import argparse
import json
import os
import statistics
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "sdk" / "python" / "src"))
sys.path.insert(0, str(HERE))
from forkbox import Sandbox  # noqa: E402
from restore import host_details, smaps, summary  # noqa: E402

ALLOC = r"""import os, time
n = {mb} << 20
buf = bytearray(b"\x01") * n          # every page written
open("/tmp/ready", "w").write(str(n))
time.sleep(3600)
"""


def fork_once(template: str, dirty_mb: int, n: int) -> dict:
    parent = Sandbox.create(template, ttl=1800)
    kids: list[Sandbox] = []
    try:
        parent.fs.write("/root/alloc.py", ALLOC.format(mb=dirty_mb))
        parent.exec("setsid python /root/alloc.py >/dev/null 2>&1 </dev/null &", check=True)
        deadline = time.monotonic() + 120
        while parent.exec("test -s /tmp/ready").exit_code != 0:
            if time.monotonic() > deadline:
                raise SystemExit(f"allocating {dirty_mb} MB timed out")
            time.sleep(0.1)
        kids = parent.fork(n)
        t = parent.last_fork
        pss = [smaps(k.pid)["Pss"] / 2**20 for k in kids]
        return {
            "dirty_mb_requested": dirty_mb, "n": n,
            "dirty_mb_merged": t.dirty_bytes / 2**20,
            "pause_ms": t.pause_ms, "snapshot_ms": t.snapshot_ms, "disks_ms": t.disks_ms,
            "resume_ms": t.resume_ms, "parent_paused_ms": t.parent_paused_ms, "merge_ms": t.merge_ms,
            "restore_ms": list(t.restore_ms), "total_ms": t.total_ms, "child_pss_mib": pss,
        }
    finally:
        for k in kids:
            k.destroy()
        parent.destroy()


def docker_run(image: str, runs: int) -> list[float]:
    subprocess.run(["docker", "pull", "-q", image], check=True, capture_output=True)
    out = []
    for _ in range(runs):
        t0 = time.perf_counter()
        subprocess.run(["docker", "run", "--rm", image, "python", "-c", "pass"], check=True, capture_output=True)
        out.append((time.perf_counter() - t0) * 1000)
    return out


def forkbox_bench(binary: str, mode: str, template: str, runs: int) -> list[float]:
    r = subprocess.run([binary, "bench", mode, template, "--runs", str(runs)], check=True, capture_output=True, text=True)
    return json.loads(r.stdout)["ms"]


def chart(result: dict, path: Path) -> None:
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    fig, ax = plt.subplots(figsize=(8, 5), dpi=150)
    for n in sorted({p["n"] for p in result["points"]}):
        pts = sorted((p for p in result["points"] if p["n"] == n), key=lambda p: p["dirty_mb_requested"])
        xs = [p["dirty_mb_requested"] for p in pts]
        ax.errorbar(xs, [p["total_ms"]["p50"] for p in pts],
                    yerr=[[p["total_ms"]["p50"] - p["total_ms"]["min"] for p in pts],
                          [p["total_ms"]["max"] - p["total_ms"]["p50"] for p in pts]],
                    marker="o", capsize=3, label=f"fork into N={n} (p50, min-max)")
    for (name, key), color, style in zip(
            (("cold boot", "coldboot_ms"), ("full restore, no CoW", "copyrestore_ms"), ("docker run", "docker_run_ms")),
            ("#444444", "#777777", "#aaaaaa"), ("--", ":", "-.")):
        if result["baselines"].get(key):
            ax.axhline(result["baselines"][key]["p50"], color=color, linestyle=style, linewidth=1.2, label=f"baseline: {name} (p50)")
    ax.set_xscale("log")
    ax.set_xlabel("memory dirtied by the parent just before the fork (MB)")
    ax.set_ylabel("total fork time (ms)")
    ax.set_title("Forkbox fork latency")
    ax.grid(True, which="both", alpha=0.3)
    ax.legend(fontsize=8)
    h = result["host"]
    fig.text(0.01, 0.005, f"{h['cpu']} ({h['cpus']} CPUs), {h['ram_bytes'] / 2**30:.1f} GiB, kernel {h['kernel']}, "
             f"virt {h['virtualization']}, {h['firecracker']}, data dir {h['data_dir_fs']}, {result['started_at'][:16]}Z",
             fontsize=6)
    fig.tight_layout(rect=(0, 0.03, 1, 1))
    fig.savefig(path)


def table(result: dict) -> str:
    h = result["host"]
    lines = [
        f"### fork curve: template `{result['template']}`, {result['reps']} runs per point",
        "",
        f"Host: {h['cpu']} ({h['cpus']} CPUs), {h['ram_bytes'] / 2**30:.1f} GiB RAM, kernel {h['kernel']}, "
        f"virtualization {h['virtualization']}, {h['firecracker']}, data dir on {h['data_dir_fs']}. {result['started_at']}.",
        "",
        "| dirty MB (merged) | N | parent paused ms | diff snapshot ms | merge ms | restore ms per child (p50) | total ms p50 (min-max) | child PSS MiB (p50) |",
        "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
    ]
    for p in sorted(result["points"], key=lambda p: (p["dirty_mb_requested"], p["n"])):
        lines.append(
            f"| {p['dirty_mb_requested']} ({p['dirty_mb_merged']['p50']:.0f}) | {p['n']} | {p['parent_paused_ms']['p50']:.1f} | "
            f"{p['snapshot_ms']['p50']:.1f} | {p['merge_ms']['p50']:.1f} | {p['restore_ms']['p50']:.1f} | "
            f"{p['total_ms']['p50']:.0f} ({p['total_ms']['min']:.0f}-{p['total_ms']['max']:.0f}) | {p['child_pss_mib']['p50']:.1f} |")
    lines += ["", "| baseline | p50 ms | min | max | stdev |", "| --- | ---: | ---: | ---: | ---: |"]
    for name, key in (("cold boot of the template", "coldboot_ms"), ("full-snapshot restore from a byte copy (no CoW)", "copyrestore_ms"),
                      ("`docker run` of the base image", "docker_run_ms")):
        s = result["baselines"].get(key)
        if s:
            lines.append(f"| {name} | {s['p50']:.0f} | {s['min']:.0f} | {s['max']:.0f} | {s['stdev']:.1f} |")
    return "\n".join(lines) + "\n"


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--template", default="bench")
    ap.add_argument("--image", default="python:3.12-slim", help="docker baseline image")
    ap.add_argument("--dirty", default="1,10,100,500,1000", help="MB, comma-separated")
    ap.add_argument("--n", default="1,10,50", help="children, comma-separated")
    ap.add_argument("--reps", type=int, default=3)
    ap.add_argument("--baseline-runs", type=int, default=10)
    ap.add_argument("--forkbox", default=str(HERE.parent / "bin" / "forkbox"))
    ap.add_argument("--data-dir", default=os.environ.get("FORKBOX_DATA_DIR", "/var/lib/forkbox"))
    ap.add_argument("--out", default=str(HERE / "results"))
    args = ap.parse_args()
    started = datetime.now(timezone.utc)
    points = []
    for dirty in [int(x) for x in args.dirty.split(",")]:
        for n in [int(x) for x in args.n.split(",")]:
            runs = [fork_once(args.template, dirty, n) for _ in range(args.reps)]
            agg = {"dirty_mb_requested": dirty, "n": n, "raw": runs}
            for k in ("dirty_mb_merged", "pause_ms", "snapshot_ms", "disks_ms", "resume_ms", "parent_paused_ms", "merge_ms", "total_ms"):
                agg[k] = summary([r[k] for r in runs])
            agg["restore_ms"] = summary([x for r in runs for x in r["restore_ms"]])
            agg["child_pss_mib"] = summary([x for r in runs for x in r["child_pss_mib"]])
            points.append(agg)
            print(f"dirty {dirty:5d} MB, N={n:2d}: total p50 {agg['total_ms']['p50']:8.1f} ms, parent paused p50 "
                  f"{agg['parent_paused_ms']['p50']:7.1f} ms, merged {agg['dirty_mb_merged']['p50']:.0f} MB", flush=True)
    baselines = {
        "coldboot_ms": summary(forkbox_bench(args.forkbox, "coldboot", args.template, args.baseline_runs)),
        "copyrestore_ms": summary(forkbox_bench(args.forkbox, "copyrestore", args.template, args.baseline_runs)),
    }
    try:
        baselines["docker_run_ms"] = summary(docker_run(args.image, args.baseline_runs))
    except (OSError, subprocess.CalledProcessError) as e:
        print(f"docker baseline skipped: {e}", file=sys.stderr)
    result = {"benchmark": "fork_curve", "template": args.template, "reps": args.reps, "started_at": started.isoformat(),
              "host": host_details(args.data_dir), "points": points, "baselines": baselines}
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    stamp = started.strftime("%Y%m%dT%H%M%SZ")
    (out / f"fork-curve-{stamp}.json").write_text(json.dumps(result, indent=2) + "\n")
    (out / f"fork-curve-{stamp}.md").write_text(table(result))
    chart(result, out / f"fork-curve-{stamp}.png")
    print("\n" + table(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())
