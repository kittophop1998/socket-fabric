// Isolated real PostgreSQL + NATS tests. No registry, production DB or releases.
import assert from 'node:assert/strict';
import { randomBytes } from 'node:crypto';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer } from 'node:net';
import { cleanEnvironment, failureSummary, root, run } from '../../scripts/release.mjs';
import { resolveNatsServer } from '../../scripts/test-tools.mjs';

async function unusedPort() {
  const reservation = createServer();
  await new Promise((resolve, reject) => { reservation.once('error', reject); reservation.listen(0, '127.0.0.1', resolve); });
  const port = reservation.address().port;
  await new Promise((resolve, reject) => reservation.close(error => error ? reject(error) : resolve()));
  return port;
}

async function composeSmoke(project, compose, environmentFile, password) {
  const httpPort = await unusedPort();
  const operatorPassword = randomBytes(32).toString('hex');
  const secret = randomBytes(32).toString('hex');
  const gatewayOverride = join(compose, '..', 'gateway.yml');
  await writeFile(gatewayOverride, `services:\n  app:\n    ports: !override\n      - "127.0.0.1:${httpPort}:${httpPort}"\n`, { mode: 0o600 });
  const args = ['compose', '--project-name', project, '--env-file', environmentFile, '-f', join(root, 'docker-compose.yml'), '-f', compose, '-f', gatewayOverride];
  const env = { ...cleanEnvironment(), APP_ENV: 'test', MANAGEMENT_ENABLED: 'true', MANAGEMENT_ALLOW_INSECURE_HTTP: 'true',
    HTTP_PORT: String(httpPort), CLIENT_TOKEN_SECRET: secret, PUBLISHER_KEYS_JSON: '{}',
    DATABASE_URL: `postgres://socketfabric_test:${password}@postgres:5432/socketfabric_test?sslmode=disable` };
  const tool = ['run', '--rm', '--no-deps', '-T', '--entrypoint', '/app/socketfabric-manage', 'app'];
  await run('docker', [...args, 'build', 'app'], { env, timeout: 240_000 });
  await run('docker', [...args, ...tool, 'migrate', 'up'], { env });
  await run('docker', [...args, ...tool, 'operator', 'bootstrap-admin', '--email', 'admin@example.com', '--password-stdin'], { env, input: operatorPassword });
  const duplicate = await run('docker', [...args, ...tool, 'operator', 'bootstrap-admin', '--email', 'duplicate@example.com', '--password-stdin'], { env, input: operatorPassword, allowFailure: true });
  assert.notEqual(duplicate.code, 0, 'Docker bootstrap allowed twice');
  await run('docker', [...args, 'up', '-d', '--wait', 'app'], { env, timeout: 60_000 });
  const base = `http://127.0.0.1:${httpPort}`;
  async function request(path, body, token, method = body === undefined ? 'GET' : 'POST', status = 200) {
    const response = await fetch(base + path, { method, signal: AbortSignal.timeout(5000),
      headers: { ...(body === undefined ? {} : { 'Content-Type': 'application/json' }), ...(token ? { Authorization: 'Bearer ' + token } : {}) },
      body: body === undefined ? undefined : JSON.stringify(body) });
    assert.equal(response.status, status, 'Unexpected Docker API response: ' + method + ' ' + path);
    return status === 204 ? undefined : response.json();
  }
  const login = await request('/admin/v1/auth/login', { email: 'admin@example.com', password: operatorPassword });
  await request('/admin/v1/projects', { id: 'compose-managed', name: 'Compose managed' }, login.token, 'POST', 201);
  const credential = await request('/admin/v1/projects/compose-managed/keys', {}, login.token, 'POST', 201);
  const deadline = Date.now() + 10_000;
  while ((await request('/admin/v1/diagnostics/overview', undefined, login.token)).management.managedProjectCount !== 1) {
    assert.ok(Date.now() < deadline, 'Docker project policy convergence deadline');
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  const cliEnv = { ...cleanEnvironment(), SOCKETFABRIC_URL: base, SOCKETFABRIC_PROJECT_ID: 'compose-managed', SOCKETFABRIC_API_KEY: credential.key };
  for (const command of [['status'], ['ping'], ['config', 'validate'], ['publish', '--room', 'room', '--event', 'resource.updated', '--data', '{"status":"completed"}']]) {
    const result = await run('go', ['run', './cmd/socketfabric', ...command], { env: cliEnv });
    assert.ok(!result.stdout.includes(credential.key) && !result.stderr.includes(credential.key), 'CLI output contained key');
  }
  const invalid = await run('go', ['run', './cmd/socketfabric', 'publish', '--room', 'room', '--event', 'resource.updated', '--data', '{}'], { env: { ...cliEnv, SOCKETFABRIC_API_KEY: randomBytes(32).toString('hex') }, allowFailure: true });
  assert.notEqual(invalid.code, 0, 'CLI accepted bad publisher credentials');
  const logs = (await run('docker', [...args, 'logs', '--no-color', 'app'], { env })).stdout;
  for (const value of [operatorPassword, password, secret, credential.key, login.token]) assert.ok(!logs.includes(value), 'Docker logs contained secret');
  await request('/admin/v1/auth/logout', undefined, login.token, 'POST', 204);
  await run('docker', [...args, 'stop', 'app'], { env, timeout: 30_000 });
  const state = await run('docker', [...args, 'ps', '-a', '--format', 'json', 'app'], { env });
  const appState = JSON.parse(state.stdout.trim());
  assert.equal(appState.ExitCode, 0, 'Management-enabled Docker shutdown failed');
  await run('docker', [...args, ...tool, 'migrate', 'down', '--confirm-data-loss'], { env });
  await run('docker', [...args, ...tool, 'migrate', 'up'], { env });
  console.log('PASS actual management Docker build/bootstrap/migrate/API/CLI/shutdown and no-secret logs');
}

async function main() {
  const contractsOnly = process.argv.includes('--contracts-only');
  const natsServer = contractsOnly ? undefined : await resolveNatsServer();
  const temporary = await mkdtemp(join(tmpdir(), 'socketfabric-management-'));
  const project = 'sf-management-' + randomBytes(6).toString('hex');
  const password = randomBytes(32).toString('hex');
  const hostPort = await unusedPort();
  const compose = join(temporary, 'compose.yml');
  const environmentFile = join(temporary, '.env');
  const args = ['compose', '--project-name', project, '--env-file', environmentFile, '-f', compose];
  let started = false;
  try {
    await writeFile(environmentFile, 'POSTGRES_PASSWORD=' + password + '\n', { mode: 0o600 });
    await writeFile(compose, `services:
  postgres:
    image: postgres:18-bookworm
    environment:
      POSTGRES_USER: socketfabric_test
      POSTGRES_DB: socketfabric_test
      POSTGRES_PASSWORD: ${password}
    ports: ["127.0.0.1:${hostPort}:5432"]
    volumes: ["pgdata:/var/lib/postgresql"]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U socketfabric_test -d socketfabric_test"]
      interval: 1s
      timeout: 2s
      retries: 30
volumes:
  pgdata:
`, { mode: 0o600 });
    started = true;
    await run('docker', [...args, 'up', '-d', '--wait'], { timeout: 120_000 });
    const port = (await run('docker', [...args, 'port', 'postgres', '5432'])).stdout.trim().split(':').at(-1);
    const container = (await run('docker', [...args, 'ps', '-q', 'postgres'])).stdout.trim();
    assert.match(port, /^\d+$/); assert.match(container, /^[a-f0-9]+$/);
    const env = { ...cleanEnvironment(), GOWORK: 'off', GOTOOLCHAIN: 'local', GIN_MODE: 'release', NATS_SERVER_BIN: natsServer,
      TEST_DATABASE_URL: `postgres://socketfabric_test:${password}@127.0.0.1:${port}/socketfabric_test?sslmode=disable`,
      TEST_POSTGRES_CONTAINER: container };
    const targets = contractsOnly ? ['./internal/bootstrap'] : ['./internal/adapters/outbound/postgres', './internal/bootstrap'];
    for (const target of targets) {
      const pattern = contractsOnly ? '^TestManagementAPIAuthRBACAndDiagnostics$' : 'TestManagement';
      const result = await run('go', ['test', '-count=1', '-race', '-tags', 'integration,postgres,contracttests', '-run', pattern, '-v', target], { env, allowFailure: true, timeout: 240_000 });
      const output = (result.stdout + result.stderr).replaceAll(password, '[redacted]').replaceAll(env.TEST_DATABASE_URL, '[redacted]');
      process.stdout.write(output);
      assert.equal(result.code, 0, 'Management integration failed for ' + target + ' (see sanitized output above)');
    }
    console.log(contractsOnly ? 'PASS PostgreSQL-backed Management HTTP contract integration' : 'PASS real PostgreSQL/NATS management integration');
    await composeSmoke(project, compose, environmentFile, password);
  } finally {
    if (started) {
      await run('docker', [...args, 'down', '--volumes', '--remove-orphans'], { timeout: 60_000 });
      const remaining = (await run('docker', ['ps', '-aq', '--filter', 'label=com.docker.compose.project=' + project])).stdout.trim();
      assert.equal(remaining, '', 'Management fixture cleanup left containers');
      const volumes = (await run('docker', ['volume', 'ls', '-q', '--filter', 'label=com.docker.compose.project=' + project])).stdout.trim();
      assert.equal(volumes, '', 'Management fixture cleanup left volumes');
      const networks = (await run('docker', ['network', 'ls', '-q', '--filter', 'label=com.docker.compose.project=' + project])).stdout.trim();
      assert.equal(networks, '', 'Management fixture cleanup left networks');
    }
    await rm(temporary, { recursive: true, force: true });
  }
}
main().catch(error => { console.error(failureSummary(error)); process.exitCode = 1; });
