# The Go Programming Language Illium

Go is an open source programming language that makes it easy to build simple,
reliable, and efficient software.

This fork is a personal Go branch maintained by 404Setup.

## Features

Preserves Go syntax and standard library API compatibility.

### Compiler & Runtime
- **`cmd/compile`**: `-ldflags="-o2"` (loop unrolling, post-prove DCE), `-ldflags="-o3"` (cost-based inlining, loop interchange/tiling, cross-loop BCE, reflection/interface DCE), `-ldflags="-fmth"` (fast-math SSA), and stack SBO for non-escaping slices/strings (`all=` applies flags across dependencies).
- **`//go:immutable`**: compile-time and runtime immutability for slices, arrays, maps, `*list.List`, `*ring.Ring`, and `sync/v2/container` maps. Applies to variable declarations (`var`, `:=`) and named functions/methods (`//go:immutable [args<N>...] [r|r<N>...]`; bare `//go:immutable` constrains all supported non-variadic parameters, `r` aliases `r0` for single-result functions, verified at compile time across call and return sites).
- **`runtime`, `slices`, `maps`, `container/list`, `container/ring`**: span/stack/GC sweep optimizations, write-barrier deduplication, and `ToImmutable` / `IsImmutable`.
- **`cmd/go`, `cmd/internal/{archive,bio,buildid,goobj,obj}`**: sharded per-bucket cache indexes (`idx`) and small-blob packing (`pack`), single-pass pooled `GOCACHE` writes, 64 KiB `bio`/copy/hash buffers, skipped empty stdout and unmodified source caching, and bounds-checked LZ4 object compression (`\x00go120lz`; `__.PKGDEF` remains uncompressed).

### Standard Library
- **`sync/v2`, `sync/v2/container`, `sync/atomic/v2`**: generic `Pool[T]`, `Map[K, V]`, unboxed `atomic/v2.Value[T]`, and `LinkedHashMap`, `OrderedMap`, `TreeMap`, `ConcurrentSkipListMap` (`ToImmutable`, `IsImmutable`).
- **`encoding/base64`**: AVX2/SSSE3 (`amd64`), NEON/SVE2 (`arm64`) vector codecs, LUT4 scalar decode, and single-alloc `EncodeToString`.
- **`encoding/hex`**: AVX2/SSSE3 (`amd64`) and NEON (`arm64`) vector encode/decode, LUT16 scalar encode, AVX2/packed `Dump`, zero-copy `DecodeString`, single-alloc `EncodeToString`, and large-buffer `Decoder.Read`.
- **`encoding/json`, `encoding/json/v2`, `encoding/xml`**: compiled marshal/unmarshal plans, SWAR/SIMD scanning, pooled codecs, and generic `Append*`/`*Of` fast paths (`jsonv2` and `nojsonv2`).
- **`fmt`, `internal/fmtsort`, `io`**: stack-buffered formatting/append fast paths, specialized `[]T` and map/slice comparators, direct string scanning, and exact-sized `io.ReadAll`.
- **`net`, `net/http`, `net/http/server`, `net/url`**: TTL DNS cache (`GODEBUG=netdnscache=0`), HTTP/1.1/2/3 header/allocation optimizations, zero-alloc `net/http/server` router, and fast `url.ParseQuery`.
- **`reflect`**: fast paths for `Equal`, `DeepEqual`, `Comparable`, `Call`, and `VisibleFields`.

---

## Benchmarks

Measured on `linux/amd64` (Intel Xeon, `-cpu=2`) against upstream Go / pre-optimization baselines.

### Compiler, Runtime & `reflect`

| Area | Benchmark | Baseline | Illium | Delta |
| :--- | :--- | ---: | ---: | :--- |
| Compiler (`-o3`) | `LoopInterchange` (512x512) / `CostBasedInlining` | 1,974 µs / 163.8 ns | **295.2 µs / 25.57 ns** | **6.69× / 6.41×** |
| Compiler (`-o2`/`-fmth`) | `LoopUnrollFull` / `math.Pow` | 6.31 / 43.17 ns | **1.59 / 17.04 ns** | **3.97× / 2.53×** |
| Binary Size (`-o3`) | Service + Reflect + Crypto | 8.59 MB | **5.29 MB** | **-38.4%** |
| Stack SBO | `make([]byte, 128)` / `[]byte(s)` (48 B) | 37.86 / 22.53 ns | **3.71 / 1.88 ns** | **1 → 0 allocs** |
| Runtime / `reflect` | `GCSpanSetResetEmpty` / `reflect.Comparable` | 3,424 / 170.8 ns | **468.7 / 16.36 ns** | **7.31× / 10.44×** |
| Build (`2 vCPU`) | Cold `go build std` / `go test fmt encoding/{json,hex}` | 66.73 / 20.83 s | **62.29 / 20.12 s** | **-6.7% / -3.4%** |
| Build Cache Size | `GOCACHE` (`std` + `cmd/go` + tests) | 377.3 MB | **154.9 MB** | **-58.9%** |
| Build Cache Files | `GOCACHE` total files (`-a` / `-d` / `idx`+`pack`) | 5,085 | **1,395** | **-72.6%** |

### Concurrency (`sync/v2`, `sync/atomic/v2`)

| Benchmark | `v1` (`sync` / `atomic`) | `v2` (`sync/v2` / `atomic/v2`) | Delta | Allocs (`v1` → `v2`) |
| :--- | ---: | ---: | :--- | :---: |
| `MapLoadMostlyHits` / `MapMixed` | 12.14 / 52.23 ns | **7.11 / 38.81 ns** | **-41.4% / -25.7%** | 0/6 B → 0/**4 B** |
| `PoolValue` (32 B struct) | 25.90 ns | **6.23 ns** | **4.16×** | 1 (32 B) → **0 (0 B)** |
| `atomic.Value` `Load` / `Store` / `CAS Mismatch` (128 B) | 1.03 / 17.18 / 69.03 ns | **0.35 / 5.77 / 9.79 ns** | **2.94× / 2.98× / 7.05×** | 0/1/1 → **0 allocs** |

### Networking & HTTP (`net`, `net/http`, `net/http/server`)

| Benchmark | Baseline (`Go 1.27.3` / `ServeMux`) | Illium | Delta | Allocs |
| :--- | ---: | ---: | :--- | :---: |
| `ClientServer/h1` (`1×` / `64×`) | 24.03 / 50.46 µs | **10.02 / 13.63 µs** | **-58.3% / -73.0%** | 65/105 → **20/39** |
| `ClientServerParallel/64` (`https1`) | 818.8 µs | **29.45 µs** | **27.80×** | 795 → **58** |
| `ServerFakeConn` (`KeepAlive` / `Lite`) | 7,414 / 4,031 ns | **2,374 / 759.4 ns** | **-68.0% / -81.2%** | 22/12 → **6/2** |
| `ReverseProxy.ServeHTTP` / `DNSLookupHost` | 6,124 / 44,548 ns | **602.4 / 1,241 ns** | **10.17× / 35.9×** | 12/74 → **7/1** |
| `net/http/server` `Param1Route` / `ParallelParamRoute` | 227.0 / 117.0 ns | **83.05 / 41.55 ns** | **2.73× / 2.82×** | 1 → **0 allocs** |

### Encoding & Formatting

#### `encoding/json` & `encoding/json/v2` (Median Latency, Marshal / Unmarshal)

| Implementation | `SmallStruct` | `TwitterStatus` | `CitmCatalog` | `GolangSource` | `CanadaGeometry` | `SyntheaFhir` |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| **Illium** `json/v2` | 538 / 942 ns | 777 µs / 1.84 ms | 924 µs / 3.73 ms | **3.70 ms** / 7.81 ms | 925 / 997 µs | **6.70 ms** / 8.83 ms |
| **Illium** `json` (`jsonv2`) | 512 / 675 ns | 929 µs / **1.75 ms** | 950 µs / **3.64 ms** | 4.13 / **7.29 ms** | **900 / 971 µs** | 7.22 / 11.24 ms |
| **Illium** `json` (`nojsonv2`) | 433 / **599 ns** | 824 µs / 2.68 ms | 941 µs / 3.68 ms | 4.59 / 7.78 ms | 1.41 / 1.64 ms | 20.61 / **8.21 ms** |
| Go `1.27.3` `json/v2` | 1.16 / 2.25 µs | 2.17 / 4.11 ms | 3.10 / 8.57 ms | 11.22 / 22.04 ms | 2.07 / 4.07 ms | 21.75 / 14.18 ms |
| Go `1.27.3` `json` (`nojsonv2`) | 871 ns / 4.18 µs | 1.56 / 9.63 ms | 2.05 / 23.41 ms | 8.87 / 41.46 ms | 1.87 / 6.06 ms | 21.42 / 33.41 ms |
| `goccy/go-json/v2` `v0.12.0` | **407** / 830 ns | 616 µs / 2.18 ms | 826 µs / 4.21 ms | 4.52 / 8.58 ms | 1.20 / 1.12 ms | 7.91 / 9.14 ms |
| `bytedance/sonic` `v1.15.5` | 416 ns / 1.05 µs | **490 µs** / 2.21 ms | **799 µs** / 4.85 ms | 8.79 / 12.63 ms | 1.18 / 1.21 ms | 16.97 / 10.22 ms |

#### `encoding/base64`, `encoding/hex`, `encoding/xml` & `fmt`

| Package | Benchmark | Baseline | Illium | Speedup / Notes |
| :--- | :--- | ---: | ---: | :--- |
| `encoding/base64` | `Encode` / `Decode` (32 KB) | 42.42 / 49.04 µs | **3.31 / 4.35 µs** | **12.83× / 11.27×** (9.9 / 10.0 GB/s, 0 allocs) |
| `encoding/hex` | `Encode` / `Decode` (4 KB) | 5,413 / 3,014 ns | **327.4 / 275.5 ns** | **16.5× / 10.9×** (12.5 / 14.9 GB/s, 0 allocs) |
| `encoding/hex` | `Encode` / `Decode` (16 KB) | 21,599 / 11,701 ns | **1,283 / 1,091 ns** | **16.8× / 10.7×** (12.8 / 15.0 GB/s, 0 allocs) |
| `encoding/hex` | `EncodeToString` (8 KB) / `DecodeString` (4 KB) | 24,355 / 4,122 ns | **6,328 / 1,196 ns** | **3.85× / 3.45×** (1 alloc) |
| `encoding/hex` | `Dump` (1 KB / 16 KB) | 21,879 / 387,694 ns | **2,532 / 33,310 ns** | **8.64× / 11.6×** (3 → **1 alloc**) |
| `encoding/xml` | `Marshal` / `Unmarshal` / `EscapeTextPlain` | 7.80 / 21.11 / 12.31 µs | **1.19 / 4.54 / 0.25 µs** | **6.58× / 4.65× / 49.1×** |
| `fmt` | `SprintfEmpty` / `SprintfInt` / `SprintfSliceInt` | 66.35 / 41.91 / 530.6 ns | **3.27 / 16.59 / 116.9 ns** | **20.3× / 2.53× / 4.54×** |

---

## Notes & Policy

None of the commits will be merged upstream because I cannot guarantee the quality of my code and I do not have time 
to wait for the PR queue.

Want to set it up in Actions? We'll be releasing an action soon so you can seamlessly migrate to our fork. Until then, 
you can check out the examples in my project, which are licensed under the MPL-2.0 license. Once released as a release, 
it will be licensed under the Apache-2.0 license.

Before reporting an issue, you should confirm whether the problem originated with us or our upstream. You should not report 
bugs caused by us to our upstream.

We use streaming version updates, meaning we merge whatever our upstream updates in the master branch. 
We do not maintain LTS versions.

## Download and Install

### Binary Distributions

Official binary distributions are available at https://github.com/404Setup/go/releases.

### Install From Source

If a binary distribution is not available for your combination of
operating system and architecture, visit
https://go.dev/doc/install/source
for source installation instructions.
