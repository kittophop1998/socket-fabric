# Phase 5C implementation plan

Preserve the Phase 5B HTTP/WebSocket contracts and best-effort semantics. No
frontend, new public endpoints, durable delivery or public release is included.

1. Audit auth, sessions, project policy, publisher credentials, transport limits,
   persistence, logging, Docker and the existing regression fixtures. Record
   verified findings and distinguish residual risks from exploitable defects.
2. Resolve the NATS test prerequisite with the existing pinned server version;
   add a repeatable local tool setup. Run real NATS, PostgreSQL, SDK/CLI and
   Chromium regression suites, including the previously unexecuted suites.
3. Extend existing fixtures for missing repeated-failure/concurrent-publish,
   restart, isolation and cleanup cases. Keep resource admission bounded.
4. Establish a configurable baseline against real HTTP/WebSocket gateways in
   single and distributed modes. Record latency percentiles, throughput,
   allocations, memory and goroutines with precise measurement boundaries.
   Capture pprof artifacts before considering an optimization.
5. Apply only verified security/reliability fixes and compatible operational
   safeguards. Validate container execution, signals, health and cleanup.
6. Extend existing scripts/Makefile with fast and expensive validation suites;
   do not infer a CI provider from the placeholder Git directory.
7. Write security, performance and operations guidance plus an evidence-based
   Phase 5C report. Mark every major check PASS / FAIL / BLOCKED / NOT VERIFIED.

Initial inspection found the local NATS binary at
`/tmp/socketfabric-nats-server-2.15.1` (version checked), existing PostgreSQL and
Chromium runners, and no usable Git remote/provider. The executable's local
availability does not replace a documented provisioning command for new hosts.
