// Optional real-broker/browser conformance check. It builds the existing Core
// image and installs both SDKs into disposable external consumer projects.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { createServer } from 'node:http';
import { createServer as createTCPServer } from 'node:net';
import { mkdtemp, mkdir, readFile, readdir, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';
import { chromium } from 'playwright';
import { eventually, startApplication } from './application.mjs';
import { inspectPackage } from '../../scripts/release.mjs';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
let options;
try {
  options = parseArgs({ options: { cli: { type: 'boolean' }, 'cli-bin': { type: 'string' }, 'npm-tarball': { type: 'string' } } }).values;
} catch { throw new Error('Usage: node tests/sdk/run.mjs [--cli] [--cli-bin path] [--npm-tarball path]'); }
const withCLI = Boolean(options.cli || options['cli-bin']);
const temporary = await mkdtemp(join(tmpdir(), 'socketfabric-sdk-'));
const project = 'socketfabric-sdk-' + randomBytes(5).toString('hex');
const credentials = [randomBytes(32).toString('hex'), randomBytes(32).toString('hex'), randomBytes(32).toString('hex'), randomBytes(32).toString('hex')];
const [signingSecret, publisherA, publisherB, brokerToken] = credentials;
let browser, assets, composeStarted = false, singleStarted = false;
let environment = { ...process.env };
let singleEnvironment;
const applications = [];
const cliExecutable = options['cli-bin'] ? resolve(options['cli-bin']) : join(temporary, 'socketfabric');
// CLI subprocesses deliberately receive no signing secret or broker credentials.
const cliEnvironment = Object.fromEntries(Object.entries(process.env).filter(([name]) => !/^(SOCKETFABRIC_|CLIENT_TOKEN_SECRET|PUBLISHER_|NATS_)/.test(name)));
const redact = value => credentials.reduce((text, secret) => text.replaceAll(secret, '[redacted]'), String(value));

async function command(binary, args, options = {}) {
  const result = await new Promise((resolve, reject) => {
    const child = spawn(binary, args, { cwd: options.cwd ?? root, env: options.env ?? environment, stdio: [options.input === undefined ? 'ignore' : 'pipe', 'pipe', 'pipe'] });
    if (options.input !== undefined) child.stdin.end(options.input);
    let stdout = '', stderr = '';
    child.stdout.on('data', chunk => { stdout += chunk; });
    child.stderr.on('data', chunk => { stderr += chunk; });
    const timer = setTimeout(() => { child.kill('SIGKILL'); reject(new Error('Integration command exceeded its deadline')); }, options.timeout ?? 180_000);
    child.on('error', error => { clearTimeout(timer); reject(error); });
    child.on('close', code => { clearTimeout(timer); resolve({ code, stdout, stderr }); });
  });
  if (result.code !== 0 && !options.allowFailure) throw new Error(redact(`${binary} ${args[0]} failed: ${(result.stdout + result.stderr).slice(-4000)}`));
  return result;
}

const compose = args => command('docker', ['compose', '-f', 'docker-compose.distributed.yml', '-p', project, ...args]);
const singleCompose = args => command('docker', ['compose', '-f', 'docker-compose.yml', '-p', project + '-single', ...args], { env: singleEnvironment });
const cli = (args, options = {}) => command(cliExecutable, args, { ...options, cwd: temporary, env: { ...cliEnvironment, SOCKETFABRIC_TIMEOUT: '3s', ...options.env } });
const pause = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds));
async function unusedPort() {
  const server = createTCPServer();
  await new Promise((resolve, reject) => server.once('error', reject).listen(0, '127.0.0.1', resolve));
  const port = server.address().port;
  await new Promise(resolve => server.close(resolve));
  return port;
}
async function status(url, want) {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    try { if ((await fetch(url, { signal: AbortSignal.timeout(300) })).status === want) return; } catch { /* startup/reconnect */ }
    await pause(50);
  }
  throw new Error('Gateway did not reach expected health status ' + want);
}

async function verifyContainerSandbox(composeCommand, names) {
  for (const name of names) {
    const id = (await composeCommand(['ps', '-q', name])).stdout.trim();
    const container = JSON.parse((await command('docker', ['inspect', id])).stdout)[0];
    assert.ok(container.Config.User && !/^(?:root|0)(?::|$)/.test(container.Config.User), 'Realtime container must run as non-root');
    assert.equal(container.HostConfig.ReadonlyRootfs, true, 'Realtime container root filesystem is writable');
    assert.ok(container.HostConfig.CapDrop.includes('ALL'), 'Realtime container retains Linux capabilities');
    assert.ok(container.HostConfig.SecurityOpt.includes('no-new-privileges:true'), 'Realtime container can gain new privileges');
    assert.equal(container.HostConfig.LogConfig.Config['max-size'], '10m');
    assert.equal(container.HostConfig.LogConfig.Config['max-file'], '3');
  }
  console.log('PASS runtime container safeguards: non-root, read-only filesystem, dropped capabilities, no-new-privileges and bounded log rotation');
}

try {
  if (withCLI) {
    if (!options['cli-bin']) await command('go', ['build', '-o', cliExecutable, './cmd/socketfabric']);
    const dependencies = (await command('go', ['list', '-deps', './cmd/socketfabric'])).stdout;
    assert.ok(!/socketfabric\/internal|gin-gonic|nats-io|coder\/websocket/.test(dependencies), 'CLI imports Core infrastructure');
    const version = JSON.parse((await cli(['version', '--json'], { env: { SOCKETFABRIC_URL: 'invalid' } })).stdout);
    if (options['cli-bin']) assert.ok(typeof version.version === 'string' && version.version.length > 0);
    else assert.equal(version.version, 'dev');
    const config = JSON.parse((await cli(['config', 'validate', '--json'])).stdout);
    assert.equal(config.apiKeyConfigured, false);
    console.log('PASS CLI standalone build, independent dependency graph, external working directory and credential-free version/config');
  }
  const sdk = join(root, 'sdk/typescript');
  let tarball;
  if (options['npm-tarball']) tarball = resolve(options['npm-tarball']);
  else {
    await command('npm', ['run', 'build'], { cwd: sdk });
    const packed = JSON.parse((await command('npm', ['pack', '--json', '--pack-destination', temporary], { cwd: sdk })).stdout)[0];
    tarball = join(temporary, packed.filename);
  }
  const packed = inspectPackage(await readFile(tarball));
  assert.equal(packed.metadata.name, '@socketfabric/client');
  const consumer = join(temporary, 'browser-consumer');
  await mkdir(consumer);
  for (const file of ['package.json', 'package-lock.json', 'app.ts', 'index.html', 'tsconfig.json', 'build.mjs']) {
    await writeFile(join(consumer, file), await readFile(join(root, 'examples/sdk', file)));
  }
  await command('npm', ['ci', '--ignore-scripts', '--no-audit', '--no-fund'], { cwd: consumer });
  await command('npm', ['install', '--ignore-scripts', '--no-audit', '--no-fund', tarball], { cwd: consumer });
  await command('npm', ['run', 'build'], { cwd: consumer });
  const installed = JSON.parse(await readFile(join(consumer, 'node_modules/@socketfabric/client/package.json'), 'utf8'));
  assert.ok(!installed.dependencies || Object.keys(installed.dependencies).length === 0, 'Browser SDK introduced runtime dependencies');
  assert.equal(installed.exports['.'].import, './dist/index.js');
  assert.equal(installed.exports['.'].types, './dist/index.d.ts');
  await writeFile(join(consumer, 'ssr.mjs'), "Object.defineProperty(globalThis, 'WebSocket', { get() { throw new Error('SSR touched WebSocket'); } });\nconst sdk = await import('@socketfabric/client');\nif (typeof sdk.SocketFabricClient !== 'function') throw new Error('Missing export');\n");
  await command('node', ['ssr.mjs'], { cwd: consumer });
  await writeFile(join(consumer, 'consumer.ts'), "import { SocketFabricClient, type EventEnvelope } from '@socketfabric/client';\nconst client = new SocketFabricClient<{ value: number }>({ url: 'ws://localhost/ws', tokenProvider: () => 'client-token' });\nclient.on('event', (event: EventEnvelope<{value:number}>) => { const value: number = event.data.value; void value; });\n// @ts-expect-error room IDs are strings\nvoid client.subscribe(123);\n");
  await writeFile(join(consumer, 'tsconfig.node-consumer.json'), JSON.stringify({ compilerOptions: { noEmit: true, strict: true, module: 'NodeNext', moduleResolution: 'NodeNext', target: 'ES2022' }, files: ['consumer.ts'] }));
  await command('node', [join(consumer, 'node_modules/typescript/bin/tsc'), '-p', 'tsconfig.node-consumer.json'], { cwd: consumer });

  const backend = join(temporary, 'backend-consumer');
  await mkdir(backend);
  // Workspace modules supply the local SDK. Requiring its provisional, hostless
  // v0.0.0 path would trigger a public module lookup when resolving transitives.
  await writeFile(join(backend, 'go.mod'), 'module example.com/socketfabric-sdk-consumer\n\ngo 1.27.2\n');
  for (const file of (await readdir(join(root, 'examples/sdk'))).filter(file => file.endsWith('.go') && !file.endsWith('_test.go'))) {
    await writeFile(join(backend, file), await readFile(join(root, 'examples/sdk', file)));
  }
  const goSDK = join(temporary, 'go-sdk');
  await mkdir(goSDK);
  for (const file of (await readdir(join(root, 'sdk/go'))).filter(file => file === 'go.mod' || file === 'go.sum' || file.endsWith('.go') && !file.endsWith('_test.go'))) {
    await writeFile(join(goSDK, file), await readFile(join(root, 'sdk/go', file)));
  }
  const workspace = join(temporary, 'go.work');
  await writeFile(workspace, 'go 1.27.2\n\nuse (\n ./go-sdk\n ./backend-consumer\n)\n');
  const goEnvironment = { ...environment, GOWORK: workspace };
  const executable = join(temporary, 'backend');
  await command('go', ['build', '-o', executable, '.'], { cwd: backend, env: goEnvironment });
  assert.ok(!/\breplace\b/.test(await readFile(join(backend, 'go.mod'), 'utf8')), 'Independent consumer used a module replacement');
  const dependencies = (await command('go', ['list', '-deps', '.'], { cwd: backend, env: goEnvironment })).stdout;
  assert.ok(!/socketfabric\/internal|gin-gonic|nats-io/.test(dependencies), 'SDK imports Core infrastructure');
  console.log('PASS independent consumers: copied Go SDK via temporary workspace (public install unverified); npm tarball contents/exports/types, browser bundle and SSR-safe import');

  const distribution = join(consumer, 'node_modules/@socketfabric/client/dist');
  assets = createServer(async (request, response) => {
    try {
      const pathname = new URL(request.url, 'http://localhost').pathname;
      if (pathname === '/') {
        response.setHeader('Content-Type', 'text/html');
        response.end('<!doctype html><title>SDK protocol fixture</title>');
        return;
      }
      const modulePrefix = pathname.startsWith('/client/') ? '/client' : '/node_modules/@socketfabric/client/dist';
      const path = resolve(distribution, '.' + pathname.slice(modulePrefix.length));
      if (!pathname.startsWith(modulePrefix + '/') || !path.startsWith(distribution + sep) || !path.endsWith('.js')) { response.writeHead(404).end(); return; }
      response.setHeader('Content-Type', 'text/javascript');
      response.end(await readFile(path));
    } catch { response.writeHead(404).end(); }
  });
  await new Promise(resolve => assets.listen(0, '127.0.0.1', resolve));
  const origin = 'http://127.0.0.1:' + assets.address().port;
  const portA = await unusedPort(), portB = await unusedPort();
  assert.notEqual(portA, portB);
  const urls = ['http://127.0.0.1:' + portA, 'http://127.0.0.1:' + portB];
  const applicationOrigin = 'http://127.0.0.1:' + await unusedPort();
  const singleOrigin = 'http://127.0.0.1:' + await unusedPort();
  environment = { ...process.env, GOWORK: 'off', APP_ENV: 'test', LOG_LEVEL: 'info', CLIENT_TOKEN_SECRET: signingSecret,
    PUBLISHER_KEYS_JSON: JSON.stringify({ 'project-a': publisherA, 'project-b': publisherB }), NATS_TOKEN: brokerToken,
    GATEWAY_A_PORT: String(portA), GATEWAY_B_PORT: String(portB), WS_ALLOWED_ORIGINS: [origin, applicationOrigin, singleOrigin].join(','), DEMO_ENABLED: 'false',
    WS_IDLE_TIMEOUT: '0s', WS_PING_INTERVAL: '1s', WS_PONG_TIMEOUT: '1s', WS_WRITE_TIMEOUT: '2s', SHUTDOWN_TIMEOUT: '4s' };
  composeStarted = true;
  if (withCLI) await compose(['build']);
  await compose(['up', ...(withCLI ? [] : ['--build']), '-d', '--wait', '--wait-timeout', '45']);
  for (const url of urls) await status(url + '/readyz', 200);
  await verifyContainerSandbox(compose, ['nats', 'gateway-a', 'gateway-b']);

  async function token(projectID = 'project-a', ttl = '5m') {
    const value = (await command(executable, ['token', '--ttl', ttl], { env: { ...environment, SOCKETFABRIC_PROJECT_ID: projectID, SOCKETFABRIC_ROOMS: 'room-a,room-b' } })).stdout.trim();
    credentials.push(value);
    return value;
  }
  async function publish(index = 0, projectID = 'project-a', room = 'room-a', data = '{"value":42}') {
    const result = await command(executable, ['publish'], { env: { ...environment, SOCKETFABRIC_URL: urls[index], SOCKETFABRIC_PROJECT_ID: projectID,
      PUBLISHER_KEY: projectID === 'project-a' ? publisherA : publisherB, SOCKETFABRIC_ROOM_ID: room, SOCKETFABRIC_DATA_JSON: data } });
    return JSON.parse(result.stdout);
  }
  const browserEnvironment = Object.fromEntries(Object.entries(process.env).filter(([name]) => !/TOKEN|SECRET|PUBLISHER|KEY/.test(name)));
  browser = await chromium.launch({ headless: true, args: ['--no-sandbox'], env: browserEnvironment });
  const page = await browser.newPage();
  const pageErrors = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  await page.goto(origin);
  await page.evaluate(async () => {
    window.SDK = await import('/client/index.js');
    window.clients = {};
    window.addClient = async (id, url, credential) => {
      const entry = { credential, events: [], states: [], errors: [], tokenCalls: 0, subscriptionCount: 0, info: undefined };
      const client = new window.SDK.SocketFabricClient({ url, tokenProvider: ({signal}) => { if (signal.aborted) throw new Error('Aborted'); entry.tokenCalls++; return entry.credential; } });
      entry.client = client;
      client.on('event', event => entry.events.push(event));
      client.on('state', state => entry.states.push(state));
      client.on('error', error => entry.errors.push(error.code));
      client.on('subscribed', () => entry.subscriptionCount++);
      window.clients[id] = entry;
      entry.info = await client.connect();
      return entry.info;
    };
  });
  const aToken = await token(), bToken = await token('project-b');
  const first = await page.evaluate(async ([url, token]) => { const info = await window.addClient('a', url, token); await window.clients.a.client.subscribe('room-a'); return info; }, [urls[0].replace('http', 'ws') + '/ws', aToken]);
  await page.evaluate(async ([url, a, b]) => {
    await window.addClient('b', url, a); await window.clients.b.client.subscribe('room-a');
    await window.addClient('room', url, a); await window.clients.room.client.subscribe('room-b');
    await window.addClient('project', url, b); await window.clients.project.client.subscribe('room-a');
  }, [urls[1].replace('http', 'ws') + '/ws', aToken, bToken]);
  async function received(id, event) {
    await page.waitForFunction(([id, eventId]) => window.clients[id].events.some(event => event.eventId === eventId), [id, event.eventId]);
    assert.deepEqual(await page.evaluate(([id, eventId]) => window.clients[id].events.filter(event => event.eventId === eventId), [id, event.eventId]), [event]);
  }
  const events = [];
  for (const index of [0, 1]) {
    const result = await publish(index);
    assert.equal(result.queuedConnections, 1);
    await received('a', result.event); await received('b', result.event);
    events.push(result.event.eventId);
  }
  const otherRoom = await publish(0, 'project-a', 'room-b', 'null');
  assert.equal(otherRoom.queuedConnections, 0); await received('room', otherRoom.event);
  const otherProject = await publish(0, 'project-b');
  assert.equal(otherProject.queuedConnections, 0); await received('project', otherProject.event);
  assert.deepEqual(await page.evaluate(() => window.clients.room.events.map(event => event.eventId)), [otherRoom.event.eventId]);
  assert.deepEqual(await page.evaluate(() => window.clients.project.events.map(event => event.eventId)), [otherProject.event.eventId]);
  assert.deepEqual(await page.evaluate(() => window.clients.a.events.map(event => event.eventId)), events);
  assert.deepEqual(await page.evaluate(() => window.clients.b.events.map(event => event.eventId)), events);
  const forbidden = await page.evaluate(async () => { try { await window.clients.a.client.subscribe('denied'); return 'accepted'; } catch (error) { return error.code; } });
  assert.equal(forbidden, 'forbidden');
  console.log('PASS SDK flow: SDK-signed tokens, Go publish to either node, local counts, null payload, room/project isolation and forbidden join');

  async function cliPublish(url, data = '{"value":"CLI-private-payload"}', key = publisherA, options = {}) {
    const result = await cli(['publish', '--room', 'room-a', '--event', 'resource.updated', '--data', '-', '--json'], {
      ...options, input: data, env: { SOCKETFABRIC_URL: url, SOCKETFABRIC_PROJECT_ID: 'project-a', SOCKETFABRIC_API_KEY: key },
    });
    for (const secret of credentials) assert.ok(!(result.stdout + result.stderr).includes(secret), 'CLI exposed a credential');
    assert.ok(!(result.stdout + result.stderr).includes('CLI-private-payload'), 'CLI exposed event data');
    return result;
  }
  async function checkCLIEvent(url, recipients, localCount) {
    const result = JSON.parse((await cliPublish(url)).stdout);
    assert.equal(result.accepted, true); assert.equal(result.deliveryConfirmed, false);
    assert.equal(result.queuedConnections, localCount); assert.ok(!('data' in result));
    const event = { eventId: result.eventId, projectId: result.projectId, roomId: result.roomId,
      type: result.type, timestamp: result.timestamp, data: { value: 'CLI-private-payload' } };
    for (const id of recipients) await received(id, event);
    assert.equal(await page.evaluate(eventId => ['room', 'project'].some(id => window.clients[id].events.some(event => event.eventId === eventId)), event.eventId), false);
  }
  const invalidKey = randomBytes(32).toString('hex'); credentials.push(invalidKey);
  if (withCLI) {
    for (const url of urls) {
      const result = JSON.parse((await cli(['status', '--url', url, '--json'])).stdout);
      assert.equal(result.scope, 'gateway'); assert.equal(result.health.ok, true); assert.equal(result.readiness.ok, true);
      assert.equal(JSON.parse((await cli(['ping', '--url', url, '--json'])).stdout).http.httpStatus, 200);
      await checkCLIEvent(url, ['a', 'b'], 1);
    }
    const badAuth = await cliPublish(urls[0], 'null', invalidKey, { allowFailure: true });
    assert.equal(badAuth.code, 1); assert.equal(JSON.parse(badAuth.stderr).error.httpStatus, 401);
    const forbidden = await cliPublish(urls[0], 'null', publisherB, { allowFailure: true });
    assert.equal(forbidden.code, 1); assert.equal(JSON.parse(forbidden.stderr).error.httpStatus, 403);

  }

  const users = {
    alice: { password: randomBytes(32).toString('hex'), projectId: 'project-a', rooms: ['room-a'] },
    bob: { password: randomBytes(32).toString('hex'), projectId: 'project-a', rooms: ['room-b'] },
    carol: { password: randomBytes(32).toString('hex'), projectId: 'project-b', rooms: ['room-a'] },
  };
  credentials.push(...Object.values(users).map(user => user.password));
  const makeApplication = (appOrigin, gatewayURL, gatewayURLs, tokenTTL) => {
    const application = startApplication({ executable, cwd: backend, publicDir: join(consumer, 'public'), origin: appOrigin,
      gatewayURL, gatewayURLs, signingSecret, publisherKeys: { 'project-a': publisherA, 'project-b': publisherB }, users, tokenTTL,
      rememberCredential: credential => credentials.push(credential) });
    applications.push(application);
    return application;
  };
  async function businessPublish(application, resourceId = 'resource-42', username = 'alice', roomId = 'room-a') {
    const response = await application.request('/api/resources/' + resourceId + '/update', { username, body: { roomId, status: 'completed' } });
    assert.equal(response.status, 202);
    return (await response.json()).event;
  }
  async function publishFromUI(session, resourceId) {
    await session.page.fill('#resource', resourceId);
    const response = session.page.waitForResponse(response => response.url().endsWith('/api/resources/' + resourceId + '/update'));
    await session.page.click('#publish');
    const accepted = await response;
    assert.equal(accepted.status(), 202);
    return (await accepted.json()).event;
  }

  const singlePort = await unusedPort();
  assert.ok(![portA, portB].includes(singlePort));
  const singleURL = 'http://127.0.0.1:' + singlePort;
  singleEnvironment = { ...environment, SOCKETFABRIC_MODE: 'single', HTTP_HOST: '0.0.0.0', HTTP_PORT: String(singlePort) };
  singleStarted = true;
  await singleCompose(['build']);
  await singleCompose(['up', '-d', '--wait', '--wait-timeout', '45']);
  await status(singleURL + '/readyz', 200);
  await verifyContainerSandbox(singleCompose, ['app']);
  await page.evaluate(async ([url, token]) => { await window.addClient('single', url, token); await window.clients.single.client.subscribe('room-a'); }, [singleURL.replace('http', 'ws') + '/ws', aToken]);
  if (withCLI) {
    assert.equal(JSON.parse((await cli(['status', '--url', singleURL, '--json'])).stdout).readiness.ok, true);
    assert.equal(JSON.parse((await cli(['ping', '--url', singleURL, '--json'])).stdout).http.ok, true);
    await checkCLIEvent(singleURL, ['single'], 1);
    const singleBadAuth = await cliPublish(singleURL, 'null', invalidKey, { allowFailure: true });
    assert.equal(singleBadAuth.code, 1); assert.equal(JSON.parse(singleBadAuth.stderr).error.httpStatus, 401);
    console.log('PASS CLI single/distributed diagnostics, stdin publish to actual authorized browsers, cross-node isolation, 401/403 and secret/payload redaction');
  }
  const singleApplication = makeApplication(singleOrigin, singleURL, [singleURL.replace('http', 'ws') + '/ws'], '4s');
  await singleApplication.ready();
  const singleA = await singleApplication.session(browser, 'alice');
  const singleB = await singleApplication.session(browser, 'alice');
  const singleEvent = await publishFromUI(singleA, 'single-resource');
  await singleA.received(singleEvent); await singleB.received(singleEvent);
  // The example preserves terminal policy expiry, then explicitly obtains a fresh token.
  await singleA.page.waitForFunction(() => document.querySelector('#state').textContent === 'disconnected', undefined, { timeout: 10_000 });
  const expiredAppCalls = singleA.tokenCalls;
  await pause(700);
  assert.equal(singleA.tokenCalls, expiredAppCalls, 'Policy close caused an authentication retry storm');
  await singleA.refresh();
  await singleA.received(await businessPublish(singleApplication, 'fresh-token-resource'));
  await singleA.disconnect();
  await singleB.disconnect();
  const disconnectedCalls = singleA.tokenCalls;
  await pause(500);
  assert.equal(singleA.tokenCalls, disconnectedCalls);
  await page.evaluate(() => { window.clients.single.client.disconnect(); delete window.clients.single; });
  await singleApplication.verifySecurity(credentials.slice(0, 4));
  console.log('PASS single-node external Go business API and packed TypeScript browser bundle; two sessions, policy expiry, fresh-token explicit refresh and cleanup');

  const application = makeApplication(applicationOrigin, urls[0], urls.map(url => url.replace('http', 'ws') + '/ws'));
  await application.ready();
  for (const [path, options, code] of [
    ['/api/realtime-token', {}, 401],
    ['/api/realtime-token', { username: 'unknown' }, 401],
    ['/api/realtime-token', { username: 'alice', body: { projectId: 'project-b', rooms: ['room-b'] } }, 400],
    ['/api/realtime-token', { username: 'alice', headers: { Origin: 'http://untrusted.invalid' } }, 403],
    ['/api/resources/resource-42/update', { username: 'alice', body: { roomId: 'room-b', status: 'completed' } }, 403],
    ['/api/resources/resource-42/update', { username: 'alice', body: { roomId: 'room-a', status: 'completed', projectId: 'project-b' } }, 400],
    ['/.env', { method: 'GET' }, 404], ['/main.go', { method: 'GET' }, 404], ['/go.mod', { method: 'GET' }, 404],
    ['/node_modules/@socketfabric/client/dist/index.js', { method: 'GET' }, 404],
  ]) assert.equal((await application.request(path, options)).status, code);
  const appA = await application.session(browser, 'alice', 0);
  const appB = await application.session(browser, 'alice', 1);
  const appRoom = await application.session(browser, 'bob', 1, 'room-b');
  const appProject = await application.session(browser, 'carol', 1);
  const applicationEvent = await publishFromUI(appA, 'distributed-resource');
  await appA.received(applicationEvent); await appB.received(applicationEvent);
  assert.equal(await appA.page.textContent('#resource-state'), 'distributed-resource: completed');
  assert.equal(await appB.page.textContent('#resource-state'), 'distributed-resource: completed');
  const roomEvent = await businessPublish(application, 'room-resource', 'bob', 'room-b');
  await appRoom.received(roomEvent);
  const projectEvent = await businessPublish(application, 'project-resource', 'carol');
  await appProject.received(projectEvent);
  await pause(150);
  for (const session of [appA, appB]) assert.deepEqual(session.frames.filter(frame => frame.op === 'event').map(frame => frame.event.eventId), [applicationEvent.eventId]);
  assert.deepEqual(appRoom.frames.filter(frame => frame.op === 'event').map(frame => frame.event.eventId), [roomEvent.eventId]);
  assert.deepEqual(appProject.frames.filter(frame => frame.op === 'event').map(frame => frame.event.eventId), [projectEvent.eventId]);
  await appA.page.fill('#room', 'room-b');
  await appA.page.click('#subscribe');
  await appA.page.waitForFunction(() => document.querySelector('#events').textContent.includes('"error":"forbidden"'));
  await appA.page.fill('#room', 'room-a');
  await appA.page.click('#subscribe'); // Already joined: the SDK/server must remain idempotent.
  await appB.page.click('#unsubscribe');
  await eventually(() => appB.frames.some(frame => frame.op === 'left'), 'Unsubscribe was not acknowledged');
  const unsubscribedEvent = await businessPublish(application, 'unsubscribed-resource');
  await appA.received(unsubscribedEvent);
  await pause(150);
  assert.ok(!appB.frames.some(frame => frame.op === 'event' && frame.event.eventId === unsubscribedEvent.eventId));
  await appB.page.click('#subscribe');
  await eventually(() => appB.frames.filter(frame => frame.op === 'joined').length === 2, 'Re-subscribe did not restore membership');
  await appA.refresh();
  const refreshedEvent = await businessPublish(application, 'refreshed-resource');
  await appA.received(refreshedEvent); await appB.received(refreshedEvent);
  const unsafeEvent = await publish(0, 'project-a', 'room-a', JSON.stringify({ resourceId: '<img src=x onerror="window.injected=true">', status: 'completed' }));
  await received('a', unsafeEvent.event);
  await pause(150);
  assert.equal(await appA.page.textContent('#resource-state'), 'refreshed-resource: completed');
  assert.equal(await appA.page.evaluate(() => Boolean(window.injected)), false);
  await appRoom.disconnect(); await appProject.disconnect();
  const appCallsBeforeRestart = appA.tokenCalls;
  const appSubscriptionsBeforeRestart = appA.frames.filter(frame => frame.op === 'joined').length;
  console.log('PASS distributed application: two actual browser sessions on different gateways, server-owned scopes, room/project isolation, auth rejection, idempotent subscriptions, explicit refresh and safe rendering');

  await compose(['stop', '-t', '8', 'gateway-a']);
  await status(urls[1] + '/readyz', 200);
  assert.equal((await application.request('/api/resources/offline-resource/update', { username: 'alice', body: { roomId: 'room-a', status: 'updated' } })).status, 503);
  await compose(['start', 'gateway-a']);
  await status(urls[0] + '/readyz', 200);
  await page.waitForFunction(() => window.clients.a.tokenCalls > 1 && window.clients.a.states.at(-1) === 'connected' && window.clients.a.subscriptionCount >= 2);
  const recovered = await publish(1);
  // Restored membership ACKs may follow the authenticated/connected state.
  await received('a', recovered.event); await received('b', recovered.event);
  await eventually(() => appA.tokenCalls > appCallsBeforeRestart && appA.frames.filter(frame => frame.op === 'joined').length > appSubscriptionsBeforeRestart && appA.sockets === 1, 'Gateway recovery did not fetch a fresh application token and restore membership');
  const appRecovered = await businessPublish(application, 'reconnected-resource');
  await appA.received(appRecovered); await appB.received(appRecovered);
  const beforeCrash = { calls: appA.tokenCalls, subscriptions: appA.frames.filter(frame => frame.op === 'joined').length,
    sdkCalls: await page.evaluate(() => window.clients.a.tokenCalls), sdkJoins: await page.evaluate(() => window.clients.a.subscriptionCount) };
  await compose(['kill', '-s', 'SIGKILL', 'gateway-a']);
  await status(urls[1] + '/readyz', 200);
  await compose(['start', 'gateway-a']);
  await status(urls[0] + '/readyz', 200);
  await eventually(() => appA.tokenCalls > beforeCrash.calls && appA.frames.filter(frame => frame.op === 'joined').length > beforeCrash.subscriptions && appA.sockets === 1, 'Crash recovery did not reauthenticate and restore a single subscription');
  await page.waitForFunction(([calls, joins]) => window.clients.a.tokenCalls > calls && window.clients.a.subscriptionCount > joins && window.clients.a.states.at(-1) === 'connected', [beforeCrash.sdkCalls, beforeCrash.sdkJoins]);
  const afterCrash = await publish(1);
  await received('a', afterCrash.event); await received('b', afterCrash.event);
  const appAfterCrash = await businessPublish(application, 'crash-recovered-resource');
  await appA.received(appAfterCrash); await appB.received(appAfterCrash);
  assert.ok(!await page.evaluate(eventId => window.clients.room.events.some(event => event.eventId === eventId) || window.clients.project.events.some(event => event.eventId === eventId), afterCrash.event.eventId), 'Crash recovery leaked across room/project scopes');
  console.log('PASS abrupt SIGKILL/restart: peer availability, fresh-token browser recovery, one active application socket and scoped delivery without echoes');
  const counts = await page.evaluate(() => [window.clients.a.tokenCalls, window.clients.b.tokenCalls]);
  const applicationCounts = [appA.tokenCalls, appB.tokenCalls];
  await compose(['stop', '-t', '5', 'nats']);
  for (const url of urls) { await status(url + '/readyz', 503); await status(url + '/healthz', 200); }
  const unavailable = await command(executable, ['publish'], { allowFailure: true, env: { ...environment, SOCKETFABRIC_URL: urls[0], PUBLISHER_KEY: publisherA } });
  assert.notEqual(unavailable.code, 0); assert.match(unavailable.stderr, /HTTP 503/);
  assert.equal((await application.request('/api/resources/broker-offline/update', { username: 'alice', body: { roomId: 'room-a', status: 'updated' } })).status, 503);
  if (withCLI) {
    for (const url of urls) {
      const statusResult = await cli(['status', '--url', url, '--json'], { allowFailure: true });
      assert.equal(statusResult.code, 1);
      const result = JSON.parse(statusResult.stdout);
      assert.equal(result.health.ok, true); assert.equal(result.readiness.httpStatus, 503); assert.equal(result.readiness.ok, false);
      assert.equal(JSON.parse((await cli(['ping', '--url', url, '--json'])).stdout).http.ok, true);
      const rejected = await cliPublish(url, 'null', publisherA, { allowFailure: true });
      assert.equal(rejected.code, 1); assert.equal(JSON.parse(rejected.stderr).error.httpStatus, 503);
    }
    console.log('PASS CLI broker outage: liveness/ping remain HTTP-only, readiness fails and publish reports HTTP 503');
  }
  await compose(['start', 'nats']);
  for (const url of urls) await status(url + '/readyz', 200);
  const resumed = await publish(0);
  await received('a', resumed.event); await received('b', resumed.event);
  assert.deepEqual(await page.evaluate(() => [window.clients.a.tokenCalls, window.clients.b.tokenCalls]), counts);
  const appResumed = await businessPublish(application, 'broker-recovered-resource');
  await appA.received(appResumed); await appB.received(appResumed);
  assert.deepEqual([appA.tokenCalls, appB.tokenCalls], applicationCounts);
  assert.equal(appRoom.tokenCalls, 1, 'Intentional disconnect resumed after gateway recovery');
  assert.equal(appProject.tokenCalls, 1, 'Intentional disconnect resumed after gateway recovery');
  await appA.disconnect(); await appB.disconnect();
  await application.verifySecurity(credentials.slice(0, 4));
  console.log('PASS recovery: real application fetches fresh tokens/re-subscribes after gateway restart; SDK and business API reject unavailable gateway/broker, retained sockets resume without replay');

  const expiring = await token('project-a', '3s');
  await page.evaluate(async ([url, token]) => { await window.addClient('expiry', url, token); await window.clients.expiry.client.subscribe('room-a'); }, [urls[0].replace('http', 'ws') + '/ws', expiring]);
  await page.waitForFunction(() => window.clients.expiry.states.at(-1) === 'disconnected', undefined, { timeout: 10_000 });
  const expiredCalls = await page.evaluate(() => window.clients.expiry.tokenCalls);
  await pause(1000);
  assert.equal(await page.evaluate(() => window.clients.expiry.tokenCalls), expiredCalls);
  const expiredAuthentication = await page.evaluate(async () => {
    try { await window.clients.expiry.client.connect(); return 'accepted'; } catch (error) { return error.code; }
  });
  assert.equal(expiredAuthentication, 'unauthorized');
  const fresh = await token();
  await page.evaluate(async token => { window.clients.expiry.credential = token; await window.clients.expiry.client.connect(); window.clients.expiry.client.disconnect(); }, fresh);
  const offline = await page.evaluate(async () => {
    window.clients.a.client.disconnect();
    await window.clients.a.client.unsubscribe('room-a');
    try { await window.clients.a.client.subscribe('room-a'); return 'accepted'; } catch (error) { return error.code; }
  });
  assert.equal(offline, 'not_connected');
  console.log('PASS lifecycle: expiry stops auth retry, fresh-token explicit connect works, offline subscribe rejects and unsubscribe clears intent');

  await compose(['stop', '-t', '8', 'gateway-a']);
  await status(urls[1] + '/readyz', 200);
  if (withCLI) {
    const unreachable = await cli(['status', '--url', urls[0], '--json'], { allowFailure: true });
    assert.equal(unreachable.code, 1); assert.equal(JSON.parse(unreachable.stdout).health.reachable, false);
    const rejected = await cliPublish(urls[0], 'null', publisherA, { allowFailure: true });
    assert.equal(rejected.code, 1); assert.equal(JSON.parse(rejected.stderr).error.code, 'network_unavailable');
    console.log('PASS CLI stopped gateway: nonzero diagnostics/publish from an external directory');
  }
  const peer = await publish(1);
  assert.equal(peer.queuedConnections, 1); await received('b', peer.event);
  await compose(['stop', '-t', '8', 'gateway-b']);
  for (const name of ['gateway-a', 'gateway-b']) {
    const id = (await compose(['ps', '-a', '-q', name])).stdout.trim();
    assert.equal(JSON.parse((await command('docker', ['inspect', id])).stdout)[0].State.ExitCode, 0);
  }
  await page.evaluate(() => Object.values(window.clients).forEach(entry => entry.client.disconnect()));
  assert.deepEqual(pageErrors, []);
  const logs = (await compose(['logs', '--no-color'])).stdout;
  const dom = await page.content();
  for (const secret of credentials.slice(0, 4)) { assert.ok(!logs.includes(secret)); assert.ok(!dom.includes(secret)); }
  const singleLogs = (await singleCompose(['logs', '--no-color'])).stdout;
  for (const secret of credentials) {
    assert.ok(!singleLogs.includes(secret) && !logs.includes(secret), 'Gateway logs exposed a credential');
    for (const application of applications) assert.ok(!application.logs.includes(secret), 'Application logs exposed a credential');
  }
  assert.equal(await page.evaluate(() => localStorage.length), 0);
  assert.notEqual(first.connectionId, '');
  console.log('PASS shutdown: peer remains operational, gateways exit 0, no page errors or server credentials in logs/browser DOM');
} catch (error) {
  console.error(redact(error.stack ?? error.message));
  process.exitCode = 1;
} finally {
  if (browser) await browser.close();
  for (const application of applications) {
    try { await application.close(); } catch (error) { console.error(redact(error.message)); process.exitCode = 1; }
  }
  if (assets) { assets.closeAllConnections(); await new Promise(resolve => assets.close(resolve)); }
  if (composeStarted) {
    try {
      await compose(['down']);
      assert.equal((await compose(['ps', '--all', '--quiet'])).stdout.trim(), '', 'Distributed containers remain after cleanup');
      assert.equal((await command('docker', ['network', 'ls', '--filter', 'label=com.docker.compose.project=' + project, '--format', '{{.Name}}'])).stdout.trim(), '', 'Distributed network remains after cleanup');
    } catch (error) { console.error(redact(error.message)); process.exitCode = 1; }
  }
  if (singleStarted) {
    try {
      await singleCompose(['down']);
      assert.equal((await singleCompose(['ps', '--all', '--quiet'])).stdout.trim(), '', 'Single-node containers remain after cleanup');
      assert.equal((await command('docker', ['network', 'ls', '--filter', 'label=com.docker.compose.project=' + project + '-single', '--format', '{{.Name}}'])).stdout.trim(), '', 'Single-node network remains after cleanup');
    } catch (error) { console.error(redact(error.message)); process.exitCode = 1; }
  }
  await rm(temporary, { recursive: true, force: true });
}
