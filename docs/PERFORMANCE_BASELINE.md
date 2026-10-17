# Phase 5C performance baseline

This is a measured local baseline, not a production capacity/SLO claim. No
production-path optimization was justified from this short workload. The CPU
profile is dominated by syscalls, JSON string processing and GC across the
Gateway **and load generator**, so it cannot establish a Gateway-only bottleneck.

## Reproduction and measurement boundaries

```sh
make test-tools
node scripts/benchmark.mjs --out bin/performance-baseline --iterations 200 --clients 32 --payload 256 --procs 4
# Longer repeat and a matched large-payload control for the slow-peer case:
node scripts/benchmark.mjs --out bin/performance-repeat --iterations 1000 --clients 32 --payload 256 --procs 4
node scripts/benchmark.mjs --out bin/performance-large-control --iterations 200 --clients 8 --payload 32768 --procs 4
```

Each output directory must be new. The runner builds actual Go bootstrap
Gateways, calls their real TCP HTTP/WS APIs through the public Go SDK and runs
NATS Core as a separate authenticated process in distributed mode. There are one
and two Gateway instances respectively; 32 clients split evenly between nodes
in the default fanout case. Management/PostgreSQL and TLS are excluded from this
performance workload and covered separately by integration tests.

Publish-only and reconnect lifecycle cases use Go's bounded parallel benchmark
workers (`GOMAXPROCS=4`, at most 32 HTTP connections per publisher). Fanout is
closed-loop: one logical event is published, then all healthy readers must see
that exact ID before the next event. Unexpected, missing or duplicated frames
fail the benchmark. The slow-peer case deliberately stops reading one additional
socket and uses up to eight healthy clients with 32 KiB of padding; compare only
with the matching eight-client/32-KiB control, not the default small workload.

HTTP latency measures SDK request start through API response/decode. E2E latency
measures the sender's start to the client reader after frame JSON decoding. All
timing uses one host; no cross-machine clock synchronization is involved.
P50/P95/P99 use nearest-rank samples (maximum 65,536 samples per case), not a
latency promise. Harness synchronization is not a client-delivery ACK protocol.

`B/op`, allocation counts, CPU percentage, post-GC heap growth per connection and
goroutine growth include **both Gateways and client load generator in one Go
process**. They exclude the external NATS process. CPU percentage can exceed
100% on four cores. Heap/connection is a before/after process delta, not the
Gateway-only connection cost or peak RSS. Setup/teardown is excluded from timed
benchmark regions; the CPU/heap profiles include the whole benchmark process.

Environment: Linux amd64, kernel 7.0.0-38-generic, Intel Pentium Gold G5400T
3.10 GHz, four logical CPUs, host memory 16,088,510,464 bytes, Go 1.27.2,
Node 24.18.0, NATS server 2.15.1. Cgroup CPU/memory limit files were unavailable
to this process and are recorded **NOT VERIFIED**, rather than inferred from
host memory. No container resource-limit or remote-network capacity conclusion
is made from this host-process experiment.

## Initial 200-event baseline (2026-10-16 local run)

| Workload | Single | Two Gateways + NATS |
| --- | ---: | ---: |
| Concurrent publish, no subscribers: accepted events/s | 14,329 | 6,960 |
| HTTP acceptance P50 / P95 / P99 (microseconds) | 197 / 600 / 1,381 | 479 / 1,146 / 1,625 |
| Fanout: logical events/s, 32 clients | 1,313 | 997 |
| Fanout: observed deliveries/s | 42,021 | 31,913 |
| E2E P50 / P95 / P99 (microseconds) | 557 / 1,120 / 1,640 | 738 / 1,439 / 2,201 |
| Fanout harness heap delta/connection (bytes) | 27,254 | 26,806 |
| Fanout harness goroutine delta/connection | 4.0 | 4.0 |
| Fanout harness CPU percentage | 283.7% | 229.0% |
| Fanout harness bytes / allocations per logical event | 142,496 / 1,083 | 147,898 / 1,131 |
| Fresh connect/auth/join P50 / P95 / P99 (microseconds) | 930 / 1,979 / 2,607 | 1,019 / 2,074 / 3,004 |

Both slow-peer cases disconnected the one unread peer while all healthy clients
continued receiving the test events. Their measured delivery P99 was 3,544 µs
(single) and 3,972 µs (distributed), with a different large-payload workload;
these numbers alone do not quantify the overhead of adding a slow peer.

CPU profile: 18.97% flat samples in Linux syscalls and 18.08% in JSON simple
string processing; the remaining prominent activity includes memory copy/GC.
These combined-harness samples do not justify adding encoded-event abstractions,
buffer pools or changing routing semantics. `bin/performance-*/results.json`,
`output.txt`, `cpu.pprof`, `heap.pprof`, and `cpu-top.txt` retain the local raw
evidence; generated artifacts are ignored by source control.

## Longer repeat and matched control (2026-10-17)

The 1,000-iteration repeat passed every workload and again disconnected one
unread peer per mode. It records 32,000 healthy deliveries in each ordinary
fanout case and 8,000 in each slow-peer case.

| Workload / metric | Single | Two Gateways + NATS |
| --- | ---: | ---: |
| No-subscriber accepted events/s | 15,997 | 6,513 |
| No-subscriber HTTP P50 / P95 / P99 (µs) | 183 / 622 / 1,258 | 527 / 1,168 / 1,707 |
| 32-client fanout logical events/s | 1,242 | 1,095 |
| Observed deliveries/s | 39,754 | 35,036 |
| Fanout E2E P50 / P95 / P99 (µs) | 567 / 1,177 / 1,771 | 699 / 1,162 / 1,875 |
| Harness heap delta / connection (bytes) | 27,713 | 26,320 |
| Harness goroutines / connection | 4.0 | 4.0 |
| Harness CPU | 289.8% | 244.9% |
| Harness bytes / allocations per fanout event | 141,448 / 1,082 | 147,382 / 1,130 |
| Fresh connection lifecycles/s | 3,293 | 3,418 |
| Connect/auth/join P50 / P95 / P99 (µs) | 933 / 2,336 / 4,119 | 939 / 2,196 / 3,728 |

A separate 200-event run used eight healthy clients and 32 KiB padding for both
the control and slow-peer case:

| Mode | Control events/s / E2E P99 µs | With unread peer events/s / E2E P99 µs | Unread peers disconnected |
| --- | ---: | ---: | ---: |
| Single | 336 / 7,959 | 417 / 3,316 | 1 |
| Distributed | 362 / 4,222 | 374 / 3,512 | 1 |

The short control had more scheduling/GC tail jitter than the slow-peer case;
this is not evidence that slow clients improve performance. Healthy readers
completed all deliveries and the peer was disconnected by the end of each
bounded run. Quantifying a small overhead or percentile confidence interval
would require randomized repeated samples and an isolated longer soak. These
results support bounded recovery behavior, not a latency guarantee.

## Limits and next measurement

This does not measure saturation, a production maximum connection count, a
long-running memory plateau, isolated per-node CPU/RSS, NATS CPU, managed-key
PostgreSQL overhead, TLS, a real reverse proxy, WAN loss or heterogeneous clients.
Queue-full and admission failure thresholds are enforced/tested by configured
limits, not inferred from benchmark throughput. Require an isolated multi-process
soak and workload-specific load envelope before selecting production limits.
