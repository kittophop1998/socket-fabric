// Real loopback HTTP/WS + NATS baseline. Artifacts stay in the ignored bin tree.
import assert from 'node:assert/strict';
import { access, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { cpus, platform, release, tmpdir, totalmem } from 'node:os';
import { join, resolve } from 'node:path';
import { parseArgs } from 'node:util';
import { cleanEnvironment, failureSummary, root, run } from './release.mjs';
import { resolveNatsServer } from './test-tools.mjs';

async function main() {
  assert.equal(platform(), 'linux', 'The resource benchmark currently requires Linux; other platforms are NOT VERIFIED');
  const args = parseArgs({ options: { out: { type: 'string' }, iterations: { type: 'string', default: '200' }, clients: { type: 'string', default: '32' }, payload: { type: 'string', default: '256' }, procs: { type: 'string', default: '4' } } }).values;
  const values = {};
  for (const [name, maximum] of Object.entries({ iterations: 10000, clients: 256, payload: 32768, procs: 32 })) {
    assert.match(args[name], /^[1-9][0-9]*$/, 'Invalid benchmark ' + name);
    values[name] = Number(args[name]);
    assert.ok(values[name] <= maximum, 'Benchmark ' + name + ' exceeds its bounded workload');
  }
  const out = resolve(args.out ?? join(root, 'bin/performance'));
  assert.ok(!await access(out).then(() => true, () => false), 'Choose a new --out directory; previous evidence is preserved');
  const temporary = await mkdtemp(join(tmpdir(), 'socketfabric-benchmark-'));
  await mkdir(out, { recursive: true });
  try {
    const nats = await resolveNatsServer();
    const env = { ...cleanEnvironment(), GOWORK: 'off', GOTOOLCHAIN: 'local', GIN_MODE: 'release', NATS_SERVER_BIN: nats,
      SF_BENCH_CLIENTS: String(values.clients), SF_BENCH_PAYLOAD_BYTES: String(values.payload) };
    const cpuProfile = join(out, 'cpu.pprof'), heapProfile = join(out, 'heap.pprof');
    const goArgs = ['test', '-tags', 'integration,performance', '-run', '^$', '-bench', '^BenchmarkRealtime', '-benchtime=' + values.iterations + 'x', '-benchmem', '-count=1', '-cpu=' + values.procs,
      '-cpuprofile=' + cpuProfile, '-memprofile=' + heapProfile, '-o', join(temporary, 'performance.test'), './internal/bootstrap'];
    const result = await run('go', goArgs, { env, timeout: 240_000, allowFailure: true });
    await writeFile(join(out, 'output.txt'), result.stdout + result.stderr);
    assert.equal(result.code, 0, 'Benchmark failed; see bounded output.txt (synthetic data only)');
    const benchmarks = [];
    for (const line of result.stdout.split('\n')) {
      if (!line.startsWith('Benchmark')) continue;
      const parts = line.trim().split(/\s+/);
      if (!/^\d+$/.test(parts[1] ?? '')) continue;
      const metrics = {};
      for (let i = 2; i + 1 < parts.length; i += 2) metrics[parts[i + 1]] = Number(parts[i]);
      benchmarks.push({ name: parts[0], iterations: Number(parts[1]), metrics });
    }
    assert.equal(benchmarks.length, 8, 'A single/distributed benchmark case is missing');
    const readLimit = async name => readFile('/sys/fs/cgroup/' + name, 'utf8').then(value => value.trim(), () => 'NOT VERIFIED');
    const metadata = {
      schemaVersion: 1, recordedAt: new Date().toISOString(), status: 'PASS', productionCapacityClaim: false,
      environment: { platform: platform(), kernel: release(), cpuModel: cpus()[0]?.model, logicalCPUs: cpus().length, hostMemoryBytes: totalmem(), cpuMax: await readLimit('cpu.max'), memoryMax: await readLimit('memory.max'), go: (await run('go', ['version'], { env })).stdout.trim(), node: process.version, nats: (await run(nats, ['-v'])).stdout.trim() },
      workload: { ...values, nodes: [1, 2], transport: 'loopback TCP, real HTTP/WS and external NATS Core', management: false, slowPeer: 'one deliberately unread socket; 8 healthy clients maximum; 32768-byte padding' },
      boundaries: ['HTTP samples measure Go SDK call completion, not delivery', 'E2E samples measure sender start to client reader after JSON decode', 'Gateways and SDK load generator share the Go test process', 'Heap, allocation, CPU and goroutine figures include both ends of each connection and exclude the external NATS process', 'Fanout is closed-loop, one event until all healthy recipients read it; it is not saturation capacity', 'At most 65536 latency samples per case; setup and teardown excluded from timed benchmark region', 'No persistence/replay/ACK; benchmark synchronization is test-harness behavior'],
      benchmarks,
    };
    await writeFile(join(out, 'results.json'), JSON.stringify(metadata, null, 2) + '\n');
    const profile = await run('go', ['tool', 'pprof', '-top', '-nodecount=20', cpuProfile], { env });
    await writeFile(join(out, 'cpu-top.txt'), profile.stdout + profile.stderr);
    console.log(result.stdout.trim());
    console.log('PASS reproducible baseline and profiles: ' + out);
  } finally { await rm(temporary, { recursive: true, force: true }); }
}
main().catch(error => { console.error(failureSummary(error)); process.exitCode = 1; });
