# Performance

go-faster/fs's performance targets come from [DESIGN.md](../findings/DESIGN.md)
**NFR-3**. This document publishes measured results and describes the in-repo
benchmark suite that turns those targets into CI regression gates.

## Targets (NFR-3)

On a single NVMe node (results in this table and below were measured on the
removed filesystem backend unless stated as a gate):

| Target | Result |
|---|---|
| Large-object throughput ≥ 80% of raw disk sequential bandwidth | **met** — PUT at the MD5 ceiling, GET ≈ raw read (see below) |
| Small-object (4 KiB) GET p99 < 10 ms at 5k req/s | **met with wide margin** — p99 ≈ 0.7 ms at ~150k req/s |
| PUT allocations a constant per 1 MiB block (no full-object buffering) | gated at ≤ 128 allocs per block |
| Streaming end-to-end (no full-object buffering) | gated — live heap grows < 64 MiB during a 256 MiB PUT |

## The large-object PUT ceiling is MD5, not the disk

The S3 protocol makes the object ETag the MD5 of its content, so a compliant
PUT must hash **every byte** on the write path. MD5 runs at roughly
0.7–0.8 GB/s on one core — **below** NVMe sequential write bandwidth
(several GB/s to the page cache). So large-object PUT throughput is bounded by
MD5, not the disk, and the literal reading of "≥ 80% of raw disk bandwidth" is
physically unachievable for *any* correct S3 server whenever the hash is
slower than the device.

The benchmark suite therefore gates PUT against the honest ceiling: the rate at
which the same machine can **stream the object through MD5 and write it**
(`stream + MD5 + write`, no fsync). On top of that mandatory work the engine
splits the body into 1 MiB blocks, hashes each with SHA-256 for its name, and
writes a metadata row per block and per version; it stores up to 4 blocks
concurrently, and GET reads ahead 8 blocks. The pure-disk figure is
reported alongside for transparency.

GET has no such tax (the ETag is already known), so it is gated directly
against raw sequential read and lands at ~95–105% of it.

## Measured results

Reference machine (AMD Ryzen 9 5950X, NVMe, Linux), single-node engine with
`NoSync`:

```
BenchmarkPutObject/4KiB     10 MB/s    1.1 MB/op   318 allocs/op   (~0.4 ms per PUT)
BenchmarkPutObject/1MiB    320 MB/s    2.1 MB/op   301 allocs/op
BenchmarkPutObject/64MiB   775 MB/s     69 MB/op  6044 allocs/op   (MD5-bound)
BenchmarkGetObject/4KiB     62 MB/s     15 KB/op   128 allocs/op
BenchmarkGetObject/1MiB   1260 MB/s    1.1 MB/op   133 allocs/op
BenchmarkGetObject/64MiB  5500 MB/s     68 MB/op  1166 allocs/op

NFR-3 gates:
  PUT 64MiB   : at or above 80% of the stream+MD5+write ceiling
  GET 64MiB   : at or above 80% of raw sequential read
  PUT allocs  : ~87 per 1 MiB block (budget 128)
  PUT memory  : live heap grows ≤ 7 MiB while a 256 MiB object is written
  4KiB GET    : p50 ≈ 0.35 ms, p99 ≈ 2.3 ms at ~29k req/s, 16 workers
```

Large objects run at the speed the filesystem backend did; small ones do not.
A 4 KiB PUT is two metadata commits, a block write and a quorum's worth of
bookkeeping — about 0.4 ms against the old backend's 60 µs — and a small GET
verifies a block's hash where the old backend read a file. Merging a PUT's
metadata writes into one commit is the obvious next step if small-object rates
matter.

Numbers vary with hardware; the CI gates are **machine-relative** (a ratio to
the same box's raw bandwidth) so they hold on slower shared runners.

## Running the suite

```sh
make bench-gate   # NFR-3 regression gates (sets FS_PERF_GATES; the perf CI job runs this)
make bench        # full ns/op / MB/s / allocs run for benchstat
```

The deterministic **allocation** and **memory** gates run in every `go test ./...` (including
the multi-platform CI matrix). The wall-clock **throughput** and **latency**
gates run only when `FS_PERF_GATES` is set — the `perf` workflow sets it and
runs on a GitHub-hosted runner, so treat its absolute numbers as noisy; the
gates hold because throughput is a same-machine ratio and the latency ceiling
has wide headroom. On the general macOS/windows/386 correctness matrix
absolute latency is noise and moving hundreds of MiB is wasteful, so they skip
there.

Compare two commits:

```sh
git stash && go test ./bench -run '^$' -bench . -benchmem -count 8 > old.txt
git stash pop && go test ./bench -run '^$' -bench . -benchmem -count 8 > new.txt
go tool benchstat old.txt new.txt
```

## What the gates enforce (bench/nfr3_test.go)

- **`TestNFR3PutAllocsPerBlock`** — a 256 MiB PUT may allocate at most 128
  more per 1 MiB block than a 64 KiB PUT does: a constant per block, never a
  function of how much of the body has accumulated. Deterministic and
  hardware-independent.
- **`TestNFR3PutMemoryBounded`** — the live heap, sampled under an aggressive
  GC while a 256 MiB object is written, must grow by less than 64 MiB: a PUT
  holds a few blocks, not the object. The strongest guard against a regression
  that starts buffering whole objects.
- **`TestNFR3LargeObjectThroughput`** — PUT ≥ 80% of the stream+MD5+write
  ceiling and GET ≥ 80% of raw sequential read, both measured on the same
  filesystem in the same run (a ratio, so runner speed cancels out).
- **`TestNFR3SmallObjectGetLatency`** — 4 KiB GET p99 under concurrent load,
  logged for tracking and gated at a generous CI ceiling.

The gates run on a single-node engine with fsync off (`engine.Options.NoSync`),
which is the "single NVMe node" NFR-3 is stated against.
