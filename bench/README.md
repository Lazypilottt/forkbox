# Benchmarks

Benchmarks run on real microVMs, on the KVM host, as root, with `forkboxd` running.
Results go to `bench/results/` as JSON plus a markdown table, with host details.

- `restore.py`: create-to-first-exec latency (p50, p95, variance) over 100 runs, and
  Firecracker RSS and PSS per sandbox with up to 10 alive.

```sh
sudo sdk/python/.venv/bin/python bench/restore.py --runs 100
```
