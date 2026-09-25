# Benchmarks

go-galaxy against `ansible-galaxy` on the same Galaxy collections, from a cold
and a warm cache.

<div class="grid cards" markdown>

-   **Cold cache, 100 collections**

    ---

    **23.0x** faster: 419 s -> 18 s

-   **Warm cache, 100 collections**

    ---

    **294.3x** faster: 291 s -> 0.99 s

</div>

![Install speedup by cache state and collection count](benchmark.svg)

> [!NOTE]
> Mean of 5 runs, both tools with `--no-deps`, on Linux with xfs against
> `ansible-core` 2.21.3.

## Results

| Cache | Collections | ansible-galaxy (s) | go-galaxy (s) | Speedup |
| :-- | --: | --: | --: | --: |
| cold | 1 | 7.195 | 2.723 | 2.6x |
| cold | 10 | 64.716 | 8.224 | 7.9x |
| cold | 100 | 419.146 | 18.221 | 23.0x |
| warm | 1 | 5.357 | 0.205 | 26.2x |
| warm | 10 | 25.927 | 0.278 | 93.4x |
| warm | 100 | 290.971 | 0.989 | 294.3x |

The chart and this table come from one
[go-galaxy-benchmark](#go-galaxy-benchmark) report; the chart rounds ratios to
two decimals, the table to one.

### What each tool caches

| Cached | ansible-galaxy | go-galaxy |
| :-- | :-- | :-- |
| Collection tarballs | No, deleted after the run | Yes |
| Extracted trees | No | Yes |
| API metadata | Yes, one `api.json` response cache | Yes, plus the last resolution |
| Installed files | Unpacked from each tarball | Hardlinked from the extracted tree |

A warm `ansible-galaxy` run still downloads every tarball; a warm go-galaxy run
sends no request. The warm rows compare two [cache
designs](caching.md#what-the-directory-holds), not one design done faster.

## Why your numbers will differ

| Rows | Bound by | On the test host |
| :-- | :-- | :-- |
| warm | Storage metadata work | 100 collections add over 66,000 files, symlinks and directories |
| cold | The network and Galaxy that day | 10 collections: `ansible-galaxy` runs spanned 36.7 s to 143.7 s |

> [!TIP]
> Keep the cache and install directory on one filesystem, or files are copied,
> not hardlinked. Tune [`--workers`](cli.md#concurrency) only where metadata
> work contends.

## What was measured

| Scenario | Wiped before each run | Primed | Network per run |
| :-- | :-- | :-- | :-- |
| `cold` | Cache, temporary tree, install directory | No | Everything, both tools |
| `warm` | Install directory | Once, unmeasured | `ansible-galaxy`: every tarball; go-galaxy: none |

Each run times `ansible-galaxy collection install --no-deps` or
`go-galaxy install --no-deps` over `testing/requirements-<N>.yml`: fetch plus
extract, not two resolvers. Each tool has its own cache, on one filesystem.

<details markdown>
<summary>Test host</summary>

| Item | Value |
| :-- | :-- |
| Guest | libvirt, Oracle Linux Server 10.1, kernel `6.12.0-203.76.7.5.el10uek.x86_64` |
| CPU and memory | 4 vCPU, 8 GB |
| Storage | SSD RAID6 passed through as a block device, formatted xfs |
| Tools | `ansible-galaxy [core 2.21.3]`, go-galaxy `v1.1.0-pre` (commit `2d12b2c`, go1.27.0) |
| The 66,000 objects | An earlier `testing/bench.sh` run, commit `826c765`, go1.26.7 |

</details>

## Reproduce

| Harness | Needs | Measures |
| :-- | :-- | :-- |
| [go-galaxy-benchmark](#go-galaxy-benchmark) | The two binaries | Collections, `cold` and `warm` |
| [`testing/bench.sh`](#testingbenchsh) | `hyperfine`, `python3`, MinIO for S3 | Also `frozen` (go-galaxy only, `--frozen --offline`), S3, roles, peak RSS, bytes downloaded |

Both run from a checkout:

```bash
python3 -m venv .venv && .venv/bin/pip install ansible-core
go build -o dist/ ./cmd/go-galaxy ./cmd/go-galaxy-benchmark
```

> [!TIP]
> Keep `--work-dir` and `$TMPDIR` on real disk: a `tmpfs` measures RAM.

### go-galaxy-benchmark

```bash
dist/go-galaxy-benchmark run --ansible-galaxy .venv/bin/ansible-galaxy \
  --go-galaxy dist/go-galaxy --work-dir /var/tmp/gg-bench --no-deps # (1)!
dist/go-galaxy-benchmark show --report /var/tmp/gg-bench/report.json \
  --format svg --out /var/tmp/gg-bench/benchmark.svg
```

1.  About 80 minutes on the test host.

`run` times both tools over collections in `cold` and `warm`, prints a table,
and saves each successful run's wall clock to `report.json`; a failed run only
adds to `FAILED`. `show` redraws a report as that table or the chart above,
offline.

> [!WARNING]
> `run` empties `--work-dir` first, an earlier `report.json` included, so give
> it a directory of its own.

<details markdown>
<summary>Flags</summary>

| Flag (`run` unless marked) | Variable | Default |
| :-- | :-- | :-- |
| `--ansible-galaxy` | `GGB_ANSIBLE_GALAXY` | `ansible-galaxy` from `PATH` |
| `--go-galaxy` | `GGB_GO_GALAXY` | `go-galaxy` from `PATH` |
| `--work-dir` | `GGB_WORK_DIR` | none, required; emptied by `run` |
| `--requirements-dir` | `GGB_REQUIREMENTS_DIR` | `testing` |
| `--runs` | `GGB_RUNS` | `5` |
| `--sizes` | `GGB_SIZES` | `1,10,100` |
| `--scenarios` | `GGB_SCENARIOS` | `cold,warm` |
| `--no-deps` | `GGB_NO_DEPS` | off, so both tools resolve dependencies |
| `--verbose`, `--quiet` (`-q`) | `GGB_VERBOSE`, `GGB_QUIET` | off |
| `--report` (both) | `GGB_REPORT` | `report.json` in `--work-dir` (`run`) or the current directory |
| `--format` (`show`) | `GGB_FORMAT` | `table`; `svg` for the chart |
| `--scenario` (`show`) | `GGB_SCENARIO` | every scenario; chart only |
| `--out` (`show`) | `GGB_OUT` | `benchmark.svg` |

</details>

<details markdown>
<summary>Sample output</summary>

```console
✔ ansible-galaxy cold size 1: mean 7.195s over 5 runs
✔ go-galaxy cold size 1: mean 2.723s over 5 runs
...
✔ go-galaxy warm size 100: mean 0.989s over 5 runs
✔ report written to /var/tmp/gg-bench/report.json
ansible-galaxy  ansible-galaxy [core 2.21.3]
go-galaxy       v1.1.0-pre (commit 2d12b2c, built by just @ 2026-08-22T16:32:56Z) // go1.27.0
host            linux/amd64, 4 cpus, xfs
measurement     5 runs, --no-deps

SCENARIO  SIZE  TOOL            MEAN      MIN       MAX       FAILED
cold      1     ansible-galaxy  7.195s    6.958s    7.603s    0
cold      1     go-galaxy       2.723s    2.494s    2.881s    0
cold      1     speedup         2.6x
cold      10    ansible-galaxy  64.716s   36.671s   143.697s  0
cold      10    go-galaxy       8.224s    5.320s    11.655s   0
cold      10    speedup         7.9x
cold      100   ansible-galaxy  419.146s  378.640s  456.976s  0
cold      100   go-galaxy       18.221s   16.459s   20.906s   0
cold      100   speedup         23.0x
warm      1     ansible-galaxy  5.357s    5.146s    5.857s    0
warm      1     go-galaxy       0.205s    0.191s    0.236s    0
warm      1     speedup         26.2x
warm      10    ansible-galaxy  25.927s   22.945s   28.109s   0
warm      10    go-galaxy       0.278s    0.258s    0.320s    0
warm      10    speedup         93.4x
warm      100   ansible-galaxy  290.971s  232.861s  452.386s  0
warm      100   go-galaxy       0.989s    0.914s    1.131s    0
warm      100   speedup         294.3x
```

</details>

### testing/bench.sh

```bash
docker compose -f testing/docker-compose.yaml up -d minio-svc # (1)!
testing/bench.sh # (2)!
SIZES=10 SCENARIOS="cold warm" testing/bench.sh # (3)!
```

1.  Only for the S3 scenarios; without MinIO they are skipped with a warning.
2.  Every size and scenario: about three hours.
3.  Or one size, two scenarios. See
    [every knob and output file](development.md#the-benchmark-harness).

Role figures are not published; `SCENARIOS="roles-cold roles-warm"
testing/bench.sh` measures them. `ansible-galaxy` keeps no role cache, so its
warm role runs download every role again.
