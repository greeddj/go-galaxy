# Benchmarks

go-galaxy against `ansible-galaxy` on the same Galaxy collections, from a cold
and a warm cache.

<div class="grid cards" markdown>

-   **Cold cache, 100 collections**

    ---

    **17.2x** faster: 459 s -> 27 s

-   **Warm cache, 100 collections**

    ---

    **275.2x** faster: 272 s -> 0.99 s

</div>

![Install speedup by cache state and collection count](../assets/benchmark.svg)

> [!NOTE]
> Mean of 5 runs, both tools with `--no-deps`, on Linux with xfs against
> `ansible-core` 2.21.4.

## Results

| Cache | Collections | ansible-galaxy (s) | go-galaxy (s) | Speedup |
| :-- | --: | --: | --: | --: |
| cold | 1 | 7.761 | 3.076 | 2.5x |
| cold | 10 | 43.348 | 4.173 | 10.4x |
| cold | 100 | 458.890 | 26.611 | 17.2x |
| warm | 1 | 5.966 | 0.194 | 30.8x |
| warm | 10 | 35.964 | 0.282 | 127.5x |
| warm | 100 | 272.312 | 0.989 | 275.2x |

The chart and this table come from one
[go-galaxy-benchmark](#reproduce) report. The chart rounds each
ratio to two decimals, and the table rounds it to one.

### What each tool caches

| Cached | ansible-galaxy | go-galaxy |
| :-- | :-- | :-- |
| Collection tarballs | No, deleted after the run | Yes |
| Extracted trees | No | Yes |
| API metadata | Yes, one `api.json` response cache | Yes, plus the last resolution |
| Installed files | Unpacked from each tarball | Hardlinked from the extracted tree |

A warm `ansible-galaxy` run still downloads every tarball. A warm go-galaxy
run sends no request. The warm rows compare two [cache
designs](../guides/caching.md#what-the-directory-holds), not one design done faster.

## What was measured

| Scenario | Wiped before each run | Primed | Network per run |
| :-- | :-- | :-- | :-- |
| `cold` | Cache, temporary tree, install directory | No | Everything, both tools |
| `warm` | Install directory | Once, unmeasured | `ansible-galaxy`: every tarball; go-galaxy: none |

Each run times `ansible-galaxy collection install --no-deps` or
`go-galaxy install --no-deps` over `testing/requirements-<N>.yml`: fetch plus
extract, not two resolvers. Each tool has its own cache, on one filesystem.
`ansible-galaxy` sees only its own install directory (`ANSIBLE_COLLECTIONS_PATH`
names it, `ANSIBLE_COLLECTIONS_SCAN_SYS_PATH=false`), because a collection it
finds anywhere else counts as installed and the run then installs nothing.

<details markdown>
<summary>Test host</summary>

| Item | Value |
| :-- | :-- |
| Guest | libvirt, Oracle Linux Server 10.1, kernel `6.12.0-203.76.7.5.el10uek.x86_64` |
| CPU and memory | 4 vCPU, 8 GB |
| Storage | SSD RAID6 passed through as a block device, formatted xfs |
| Tools | `ansible-galaxy [core 2.21.4]`, go-galaxy `v1.2.3-78-g7cf1db4-dirty` built with go1.27.1 from the source of commit `7cf1db4` |
| `-dirty` in the version | The harness's `ANSIBLE_COLLECTIONS_SCAN_SYS_PATH=false`, not yet committed; it landed next as `a5c0093`, which changes no go-galaxy source |
| The 66,000 objects | An earlier run, commit `826c765`, go1.26.7 |

</details>

## Why your numbers will differ

| Rows | Bound by | On the test host |
| :-- | :-- | :-- |
| warm | Storage metadata work | 100 collections add over 66,000 files, symlinks and directories |
| cold | The network and Galaxy that day | 100 collections: `ansible-galaxy` runs spanned 386.0 s to 617.4 s |

> [!TIP]
> Keep the cache and install directory on one filesystem, or files are copied,
> not hardlinked ([The local cache](../guides/caching.md#the-local-cache)). Tune
> [`--workers`](cli.md#concurrency) only where metadata work contends.

## Reproduce

`cmd/go-galaxy-benchmark` runs from a checkout:

```bash
python3 -m venv .venv && .venv/bin/pip install ansible-core
go build -o dist/ ./cmd/go-galaxy ./cmd/go-galaxy-benchmark
```

> [!WARNING]
> go-galaxy-benchmark passes your environment through to the tools it times,
> so unset `GO_GALAXY_*` and `ANSIBLE_*` variables first. An exported
> `GO_GALAXY_S3_BUCKET`, for example, turns the go-galaxy runs into S3 runs.

> [!TIP]
> Keep `--work-dir` and `$TMPDIR` on real disk: a `tmpfs` measures RAM.

```bash
dist/go-galaxy-benchmark run --ansible-galaxy .venv/bin/ansible-galaxy \
  --go-galaxy dist/go-galaxy --work-dir /var/tmp/gg-bench --no-deps # (1)!
dist/go-galaxy-benchmark show --report /var/tmp/gg-bench/report.json \
  --format svg --out /var/tmp/gg-bench/benchmark.svg
```

1.  About 80 minutes on the test host.

`run` times both tools over collections in `cold` and `warm`, prints a table,
and saves each successful run's wall clock to `report.json`. A failed run only
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
✔ ansible-galaxy cold size 1: mean 7.761s over 5 runs
✔ go-galaxy cold size 1: mean 3.076s over 5 runs
...
✔ go-galaxy warm size 100: mean 0.989s over 5 runs
✔ report written to /var/tmp/gg-bench/report.json
ansible-galaxy  ansible-galaxy [core 2.21.4]
go-galaxy       v1.2.3-78-g7cf1db4-dirty (commit 7cf1db4, built by just @ 2026-09-27T16:07:23Z) // go1.27.1
host            linux/amd64, 4 cpus, xfs
measurement     5 runs, --no-deps

SCENARIO  SIZE  TOOL            MEAN      MIN       MAX       FAILED
cold      1     ansible-galaxy  7.761s    7.462s    8.100s    0
cold      1     go-galaxy       3.076s    2.718s    3.794s    0
cold      1     speedup         2.5x
cold      10    ansible-galaxy  43.348s   39.939s   50.557s   0
cold      10    go-galaxy       4.173s    3.535s    5.274s    0
cold      10    speedup         10.4x
cold      100   ansible-galaxy  458.890s  386.008s  617.438s  0
cold      100   go-galaxy       26.611s   22.183s   37.924s   0
cold      100   speedup         17.2x
warm      1     ansible-galaxy  5.966s    5.228s    6.974s    0
warm      1     go-galaxy       0.194s    0.192s    0.199s    0
warm      1     speedup         30.8x
warm      10    ansible-galaxy  35.964s   23.499s   49.323s   0
warm      10    go-galaxy       0.282s    0.266s    0.294s    0
warm      10    speedup         127.5x
warm      100   ansible-galaxy  272.312s  240.008s  359.534s  0
warm      100   go-galaxy       0.989s    0.918s    1.060s    0
warm      100   speedup         275.2x
```

</details>
