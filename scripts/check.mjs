// Provider-independent local/CI gate. No publication, tags or deployment.
import assert from 'node:assert/strict';
import { access, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { join, relative, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { parseArgs } from 'node:util';
import { buildArtifacts, buildInputs, cleanEnvironment, failureSummary, filesUnder, root, run, scanSource, verifyArtifacts } from './release.mjs';
import { resolveNatsServer, resolveVulnerabilityScanner } from './test-tools.mjs';

const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
const results = [];
async function step(name, callback) {
  console.log('CHECK ' + name);
  await callback();
  results.push({ name, status: 'PASS' });
  console.log('PASS ' + name);
}

async function checkDocumentation() {
  let links = 0;
  for (const path of (await filesUnder(root)).filter(path => path.endsWith('.md'))) {
    const content = await readFile(path, 'utf8');
    for (const match of content.matchAll(/\[[^\]]+\]\(([^)]+)\)/g)) {
      const target = match[1].split('#')[0];
      if (!target || /^[a-z]+:/i.test(target)) continue;
      assert.ok(!target.startsWith('/'), 'Documentation must not link to developer-local absolute paths');
      await access(resolve(path, '..', target));
      links++;
    }
  }
  assert.ok(links > 0);
}

async function auditGit() {
  const git = await run('git', ['rev-parse', '--show-toplevel'], { allowFailure: true });
  if (git.code !== 0) {
    results.push({ name: 'Git provenance, canonical origin and history secret audit', status: 'BLOCKED', reason: 'No usable Git repository in this checkout' });
    console.log('BLOCKED Git provenance/history: no usable repository');
    return;
  }
  const tracked = (await run('git', ['ls-files', '-z'])).stdout.split('\0').filter(Boolean);
  const forbidden = /(^|\/)(?:\.env(?:\.(?!example$)[^/]+)?|\.npmrc|\.netrc|\.git-credentials|\.aws\/|\.codex\/)|\.(?:pem|key|p12|pfx)$/;
  assert.ok(tracked.every(path => !forbidden.test(path)), 'Tracked secret/config file requires review; no values are printed');
  results.push({ name: 'Tracked secret-file names', status: 'PASS' });
  results.push({ name: 'Full-history secret scan and ownership', status: 'NOT VERIFIED', reason: 'Requires owner-confirmed remote and a reviewed history scanner' });
}

async function main() {
  const options = parseArgs({ options: { artifacts: { type: 'string' }, suite: { type: 'string', default: 'full' } } }).values;
  assert.ok(['fast', 'integration', 'security', 'full'].includes(options.suite), 'Unknown check suite');
  const artifacts = resolve(options.artifacts ?? join(root, 'bin/release'));
  const temporary = await mkdtemp(join(tmpdir(), 'socketfabric-quality-'));
  const environment = { ...cleanEnvironment(), GOWORK: 'off', GOTOOLCHAIN: 'local' };
  try {
    await step('Current-source targeted credential scan', scanSource);
    await auditGit();
    if (options.suite === 'security') {
      await auditDependencies(environment);
      await writeSummary('security');
      return;
    }
    if (options.suite !== 'fast') environment.NATS_SERVER_BIN = await resolveNatsServer();
    await step('Release tooling security/verification tests', () => run(process.execPath, ['--test', '--test-isolation=none', 'scripts/release.test.mjs']));
    await step('Go formatting (no source mutation)', async () => {
      const files = (await filesUnder(root)).filter(path => path.endsWith('.go'));
      const formatted = await run('gofmt', ['-l', ...files]);
      assert.equal(formatted.stdout.trim(), '', 'Run go fmt in the root and nested modules');
    });
    for (const module of ['.', 'sdk/go', 'examples/sdk']) {
      const cwd = join(root, module);
      await step(`Go ${module}: tidy/verify/vet/unit/race/build`, async () => {
        await run('go', ['mod', 'tidy', '-diff'], { cwd, env: environment });
        const moduleFile = await readFile(join(cwd, 'go.mod'), 'utf8');
        if (/^replace socketfabric\/sdk\/go => /m.test(moduleFile)) {
          // Local source has no downloaded zip to verify. Preserve the rest of
          // this graph in a temporary modfile; the independent SDK graph is
          // verified separately below. Public installation remains blocked.
          const modfile = join(temporary, module === '.' ? 'root.mod' : 'example.mod');
          await writeFile(modfile, moduleFile);
          await writeFile(modfile.replace(/\.mod$/, '.sum'), await readFile(join(cwd, 'go.sum')));
          await run('go', ['mod', 'edit', '-modfile=' + modfile, '-droprequire=socketfabric/sdk/go', '-dropreplace=socketfabric/sdk/go'], { cwd, env: environment });
          await run('go', ['mod', 'verify', '-modfile=' + modfile], { cwd, env: environment });
        } else await run('go', ['mod', 'verify'], { cwd, env: environment });
        for (const args of [['vet', './...'], ['test', '-count=1', './...'], ['test', '-count=1', '-race', './...']]) await run('go', args, { cwd, env: environment });
        await run('go', module === 'examples/sdk' ? ['build', '-o', join(temporary, 'example'), '.'] : ['build', './...'], { cwd, env: environment });
      });
    }
    if (options.suite !== 'fast') {
      await step('Fresh real-NATS adapter/bootstrap race integration', () => run('go', ['test', '-count=1', '-race', '-tags', 'integration', './internal/adapters/outbound/nats', './internal/bootstrap'], { env: environment }));
      await step('Real PostgreSQL management, migrations, RBAC and NATS/outage race integration', () => run(process.execPath, ['tests/management/run.mjs'], { env: environment, timeout: 360_000 }));
    }
    for (const module of ['sdk/typescript', 'tests/sdk']) await step(`Locked npm install ${module}`, () => run(npm, ['ci', '--ignore-scripts', '--no-audit', '--no-fund'], { cwd: join(root, module) }));
    await step('Locked contract-tool install', () => run(npm, ['ci', '--ignore-scripts', '--no-audit', '--no-fund'], { cwd: join(root, 'tests/contracts') }));
    await step('OpenAPI/AsyncAPI parsing, TypeScript generation and route/schema contract tests', async () => {
      await run(process.execPath, ['tests/contracts/run.mjs']);
      await run('go', ['vet', '-tags', 'contracttests', './...'], { env: environment });
      await run('go', ['test', '-count=1', '-tags', 'contracttests', './...'], { env: environment });
    });
    await step('TypeScript strict checks and 25 SDK tests', async () => {
      await run(npm, ['run', 'typecheck'], { cwd: join(root, 'sdk/typescript') });
      await run(npm, ['test'], { cwd: join(root, 'sdk/typescript') });
    });
    await step('Existing generated demo and 9 lifecycle tests', async () => {
      await run(process.execPath, ['internal/adapters/inbound/http/demo-build.mjs', '--check']);
      await run(process.execPath, ['--test', '--test-isolation=none', 'internal/adapters/inbound/http/demo.test.mjs']);
    });
    if (options.suite !== 'full') {
      if (options.suite === 'integration') await step('External Go/npm, standalone CLI, Docker/NATS and Chromium in both modes', () => run(process.execPath, ['tests/sdk/run.mjs', '--cli'], { env: environment, timeout: 600_000 }));
      await step('Documentation relative links', checkDocumentation);
      await writeSummary(options.suite);
      return;
    }
    await auditDependencies(environment);
    let manifest;
    await step('Reproducible local artifacts / checksums / npm inspection', async () => {
      if (await access(artifacts).then(() => true, () => false)) {
        manifest = await verifyArtifacts(artifacts);
        assert.equal(manifest.inputs.sha256, (await buildInputs()).sha256, 'Artifacts are stale; build into a new --out directory');
      } else manifest = await buildArtifacts(artifacts);
    });
    const host = (await run('go', ['env', 'GOHOSTOS', 'GOHOSTARCH'], { env: environment })).stdout.trim().split(/\s+/).join('/');
    const cli = manifest.artifacts.find(artifact => artifact.kind === 'cli' && artifact.target === host);
    const tarball = manifest.artifacts.find(artifact => artifact.kind === 'npm');
    assert.ok(cli && tarball, 'No native CLI/npm artifact available');
    await step('Actual distribution: external Go/npm consumers, CLI, Docker/NATS and Chromium in both modes', () => run(process.execPath, ['tests/sdk/run.mjs', '--cli', '--cli-bin', join(artifacts, cli.path), '--npm-tarball', join(artifacts, tarball.path)], { env: environment, timeout: 600_000 }));
    await step('Documentation relative links', checkDocumentation);
    await verifyArtifacts(artifacts);
    assert.equal(manifest.inputs.sha256, (await buildInputs()).sha256, 'Build inputs changed during checks; rebuild from stable source');
    results.push({ name: 'Public ownership, licensing, tags, registry installation and native non-host platforms', status: 'BLOCKED', reason: 'See docs/RELEASE_READINESS.md; local checks are not publication approval' });
    await mkdir(join(root, 'bin'), { recursive: true });
    await writeFile(join(root, 'bin/release-verification.json'), JSON.stringify({ schemaVersion: 1, artifactDirectory: relative(root, artifacts), publicReleaseApproved: false, results }, null, 2) + '\n');
    console.log('PASS local Phase 4D quality gates. BLOCKED public release: ownership/license/provenance approvals remain.');
  } finally { await rm(temporary, { recursive: true, force: true }); }
}

async function auditDependencies(environment) {
  const evidence = join(root, 'bin/security');
  await mkdir(evidence, { recursive: true });
  for (const module of ['sdk/typescript', 'examples/sdk', 'tests/sdk', 'tests/contracts']) {
    await step(`npm dependency audit ${module}`, async () => {
      const audit = JSON.parse((await run(npm, ['audit', '--json'], { cwd: join(root, module), allowFailure: true })).stdout);
      assert.ok(audit.metadata?.vulnerabilities && !audit.error, 'npm audit did not return a complete result');
      await writeFile(join(evidence, module.replaceAll('/', '-') + '.json'), JSON.stringify(audit, null, 2) + '\n');
      assert.equal(audit.metadata.vulnerabilities.total, 0, 'npm reports dependency vulnerabilities; review before release');
    });
  }
  const scanner = await resolveVulnerabilityScanner();
  assert.ok(scanner, 'Go vulnerability audit BLOCKED: run make test-tools or set GOVULNCHECK_BIN to the pinned scanner');
  for (const module of ['.', 'sdk/go', 'examples/sdk']) await step(`Go vulnerability audit ${module}`, async () => {
    const report = await run(scanner, ['-show', 'verbose', './...'], { cwd: join(root, module), env: environment });
    await writeFile(join(evidence, module === '.' ? 'go-root.txt' : module.replaceAll('/', '-') + '.txt'), report.stdout + report.stderr);
  });
}

async function writeSummary(suite) {
  await mkdir(join(root, 'bin'), { recursive: true });
  await writeFile(join(root, 'bin/check-' + suite + '.json'), JSON.stringify({ suite, publicReleaseApproved: false, results }, null, 2) + '\n');
  console.log('PASS ' + suite + ' checks; Git/ownership and other explicitly reported blockers remain separate.');
}

main().catch(error => { console.error(failureSummary(error)); process.exitCode = 1; });
