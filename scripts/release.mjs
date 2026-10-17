// Local preparation only. This script has no publish, tag, upload or deploy step.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { access, chmod, lstat, mkdir, mkdtemp, readFile, readdir, rename, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gunzipSync } from 'node:zlib';

export const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
export const targets = ['linux/amd64', 'linux/arm64', 'darwin/amd64', 'darwin/arm64', 'windows/amd64', 'windows/arm64'];
export const digest = data => createHash('sha256').update(data).digest('hex');
function parseJSON(data, label) {
  try { return JSON.parse(data.toString('utf8')); } catch { throw new Error(`Invalid ${label} JSON; contents are not logged`); }
}
const exists = async path => access(path).then(() => true, () => false);
const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
const ignored = new Set(['.git', '.aws', '.codex', '.agents', '.npmrc', '.netrc', '.git-credentials', '.vscode', '.idea', '.DS_Store', 'node_modules', 'bin', 'dist', 'public', '.cache', 'tmp']);

export function secretsInEnvironment(environment = process.env) {
  const values = Object.entries(environment).filter(([name]) => /SECRET|TOKEN|PASSWORD|API_KEY|DATABASE_URL|^PUBLISHER_KEY$/.test(name)).map(([, value]) => value);
  for (const [name, value] of Object.entries(environment)) if (/DATABASE_URL/.test(name)) {
    try { values.push(decodeURIComponent(new URL(value).password)); } catch { /* invalid DSNs are never logged */ }
  }
  try { values.push(...Object.values(JSON.parse(environment.PUBLISHER_KEYS_JSON ?? '{}'))); } catch { /* invalid config is not logged */ }
  return values.filter(value => typeof value === 'string' && value.length >= 16);
}

export function cleanEnvironment(environment = process.env) {
  return Object.fromEntries(Object.entries(environment).filter(([name]) => !/SECRET|TOKEN|PASSWORD|API_KEY|DATABASE_URL|^DATABASE_|^MANAGEMENT_|^POSTGRES_|^PUBLISHER_|^EXAMPLE_|^SOCKETFABRIC_/.test(name)));
}

export function failureSummary(error) {
  let message = error instanceof SyntaxError ? 'Invalid JSON input; contents are not logged' : String(error.message).split('\n')[0];
  for (const secret of secretsInEnvironment()) message = message.replaceAll(secret, '[redacted]');
  return message;
}

export async function run(binary, args, options = {}) {
  const result = await new Promise((resolvePromise, reject) => {
    const child = spawn(binary, args, { cwd: options.cwd ?? root, env: options.env ?? cleanEnvironment(), stdio: [options.input === undefined ? 'ignore' : 'pipe', 'pipe', 'pipe'], windowsHide: true });
    if (options.input !== undefined) { child.stdin.on('error', () => {}); child.stdin.end(options.input); }
    let stdout = '', stderr = '', bytes = 0;
    const collect = name => chunk => {
      bytes += chunk.length;
      if (bytes > 8 << 20) { child.kill('SIGKILL'); return; }
      if (name === 'stdout') stdout += chunk; else stderr += chunk;
    };
    child.stdout.on('data', collect('stdout')); child.stderr.on('data', collect('stderr'));
    const timer = setTimeout(() => { child.kill('SIGKILL'); reject(new Error(`${binary} exceeded its deadline`)); }, options.timeout ?? 180_000);
    child.once('error', () => { clearTimeout(timer); reject(new Error(`${binary} could not start`)); });
    child.once('close', code => { clearTimeout(timer); resolvePromise({ code, stdout, stderr }); });
  });
  if (result.code !== 0 && !options.allowFailure) {
    let detail = (result.stdout + result.stderr).slice(-2400);
    for (const secret of [...secretsInEnvironment(), ...secretsInEnvironment(options.env ?? {})]) detail = detail.replaceAll(secret, '[redacted]');
    detail = detail.replace(/https?:\/\/[^\s/@]+:[^\s/@]+@/g, 'https://[redacted]@');
    throw new Error(`${binary} ${args[0]} failed (exit ${result.code}): ${detail}`);
  }
  return result;
}

export async function filesUnder(directory) {
  const files = [];
  for (const entry of (await readdir(directory, { withFileTypes: true })).sort((a, b) => a.name.localeCompare(b.name, 'en'))) {
    if (ignored.has(entry.name) || entry.name === '.env' || entry.name.startsWith('.env.') && entry.name !== '.env.example') continue;
    const path = join(directory, entry.name);
    assert.ok(!entry.isSymbolicLink(), 'Source/artifact symlinks require manual review');
    if (entry.isDirectory()) files.push(...await filesUnder(path));
    else if (entry.isFile()) files.push(path);
  }
  return files;
}

export function scanBytes(data, known = []) {
  const buffer = Buffer.isBuffer(data) ? data : Buffer.from(data);
  const text = buffer.toString('latin1');
  const findings = [];
  const rules = [
    ['private-key', /-----BEGIN (?:RSA |EC |OPENSSH |ENCRYPTED )?PRIVATE KEY-----\r?\n[A-Za-z0-9+/=]{32,}\r?\n/],
    ['github-token', /\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{60,})\b/],
    ['npm-token', /\bnpm_[A-Za-z0-9]{36,}\b/],
    ['aws-access-key', /\b(?:AKIA|ASIA)[A-Z0-9]{16}\b/],
    ['literal-jwt', /eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{20,}/],
    ['management-credential', /\bsf(?:op|pk)_[A-Z2-7]{26}\.[A-Za-z0-9_-]{43}\b/],
  ];
  for (const [label, rule] of rules) if (rule.test(text)) findings.push(label);
  for (const secret of known) {
    if ([secret, Buffer.from(secret).toString('base64'), encodeURIComponent(secret)].some(value => buffer.includes(Buffer.from(value)))) {
      findings.push('configured-credential'); break;
    }
  }
  return [...new Set(findings)];
}

export async function scanSource() {
  const files = await filesUnder(root);
  const known = secretsInEnvironment();
  for (const path of files) {
    const findings = scanBytes(await readFile(path), known);
    assert.equal(findings.length, 0, `Credential pattern requires review in ${relative(root, path)} (${findings.join(', ')})`);
  }
  return files.length;
}

// Inspect the bounded npm tar format without extracting files or following links.
export function inspectPackage(compressed, known = []) {
  assert.ok(compressed.length <= 1 << 20, 'npm artifact exceeds 1 MiB budget');
  const tar = gunzipSync(compressed, { maxOutputLength: 8 << 20 });
  const files = new Map();
  let offset = 0;
  while (offset + 512 <= tar.length) {
    const header = tar.subarray(offset, offset + 512);
    if (header.every(byte => byte === 0)) {
      assert.ok(tar.subarray(offset).every(byte => byte === 0), 'Unexpected trailing tar data');
      break;
    }
    const field = (start, end) => header.subarray(start, end).toString('utf8').replace(/\0.*$/s, '').trim();
    const octal = value => { assert.match(value, /^[0-7]+$/); return Number.parseInt(value, 8); };
    const size = octal(field(124, 136));
    const checksum = header.reduce((sum, byte, index) => sum + (index >= 148 && index < 156 ? 32 : byte), 0);
    assert.equal(checksum, octal(field(148, 156)), 'Invalid tar header checksum');
    const name = field(0, 100);
    assert.equal(field(345, 500), '', 'Tar prefixes require manual review');
    assert.ok(header[156] === 0 || header[156] === 48, 'Only regular npm files are allowed');
    assert.match(name, /^package\/(?:package\.json|README\.md|LICENSE|NOTICE|dist\/(?:index|client|runtime|types)\.(?:js|d\.ts))$/, 'Unexpected npm package path');
    assert.ok(!files.has(name), 'Duplicate npm file');
    const end = offset + 512 + size;
    assert.ok(end <= tar.length && Number.isSafeInteger(size), 'Truncated/invalid npm file');
    const data = tar.subarray(offset + 512, end);
    assert.deepEqual(scanBytes(data, known), [], `Credential pattern in npm file ${name}`);
    files.set(name, data);
    offset = Math.ceil(end / 512) * 512;
  }
  for (const name of ['package/package.json', 'package/README.md', ...['index', 'client', 'runtime', 'types'].flatMap(name => [`package/dist/${name}.js`, `package/dist/${name}.d.ts`])]) {
    assert.ok(files.has(name), `Missing npm file ${name}`);
  }
  const metadata = parseJSON(files.get('package/package.json'), 'npm package metadata');
  assert.equal(metadata.exports['.'].import, './dist/index.js');
  assert.equal(metadata.exports['.'].types, './dist/index.d.ts');
  assert.equal(metadata.types, './dist/index.d.ts');
  assert.equal(Object.keys(metadata.dependencies ?? {}).length, 0, 'Unexpected SDK runtime dependencies');
  return { metadata, files: [...files.keys()].sort(), packedBytes: compressed.length, unpackedBytes: [...files.values()].reduce((sum, data) => sum + data.length, 0) };
}

export function buildMetadata(environment = process.env) {
  const version = environment.VERSION || 'dev', commit = environment.COMMIT || 'unknown';
  assert.match(version, /^(?:dev|v?(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?)$/, 'Invalid VERSION');
  assert.match(commit, /^(?:unknown|[a-f0-9]{7,40})$/, 'Invalid COMMIT');
  const epoch = environment.SOURCE_DATE_EPOCH || '0';
  assert.match(epoch, /^(?:0|[1-9]\d{0,11})$/, 'Invalid SOURCE_DATE_EPOCH');
  const time = new Date(Number(epoch) * 1000);
  assert.ok(Number.isFinite(time.valueOf()) && time.getUTCFullYear() <= 9999, 'SOURCE_DATE_EPOCH is out of range');
  const buildTime = environment.BUILD_TIME && environment.BUILD_TIME !== 'unknown' ? environment.BUILD_TIME : time.toISOString().replace('.000Z', 'Z');
  assert.match(buildTime, /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/, 'BUILD_TIME must be UTC RFC3339 seconds');
  assert.equal(new Date(buildTime).toISOString().replace('.000Z', 'Z'), buildTime, 'Invalid BUILD_TIME');
  return { version, commit, buildTime, sourceDateEpoch: Number(epoch) };
}

export async function buildInputs() {
  const paths = ['go.mod', 'go.sum', 'sdk/go/go.mod', 'sdk/go/go.sum', 'sdk/typescript/package.json', 'sdk/typescript/package-lock.json', 'sdk/typescript/tsconfig.json', 'sdk/typescript/README.md', 'scripts/release.mjs', 'Makefile'];
  for (const folder of ['cmd/socketfabric', 'sdk/go', 'sdk/typescript/src']) {
    for (const path of await filesUnder(join(root, folder))) if (/\.(?:go|ts)$/.test(path) && !path.endsWith('_test.go')) paths.push(relative(root, path).split(sep).join('/'));
  }
  const inputs = [];
  for (const path of [...new Set(paths)].sort()) inputs.push({ path, sha256: digest(await readFile(join(root, path))) });
  return { sha256: digest(JSON.stringify(inputs)), files: inputs };
}

export async function verifyArtifacts(directory) {
  directory = resolve(directory);
  assert.ok(!(await lstat(directory)).isSymbolicLink(), 'Artifact symlinks are forbidden');
  async function checkedPath(name) {
    assert.ok(!name.startsWith('/') && !name.split('/').some(part => part === '..' || part === '.' || part === ''), 'Unsafe checksum path');
    let path = directory;
    for (const part of name.split('/')) {
      path = join(path, part);
      assert.ok(!(await lstat(path)).isSymbolicLink(), 'Artifact symlinks are forbidden');
    }
    return path;
  }
  const manifest = parseJSON(await readFile(await checkedPath('manifest.json')), 'artifact manifest');
  assert.equal(manifest.schemaVersion, 1); assert.equal(manifest.publicReleaseApproved, false);
  const sums = (await readFile(await checkedPath('SHA256SUMS'), 'utf8')).trim().split('\n');
  const expected = [...manifest.artifacts.map(artifact => artifact.path), 'manifest.json'].sort();
  const seen = [];
  for (const line of sums) {
    const match = /^([a-f0-9]{64})  ([A-Za-z0-9_./-]+)$/.exec(line);
    assert.ok(match, 'Invalid SHA256SUMS entry');
    const [, hash, name] = match;
    assert.ok(!name.startsWith('/') && !name.split('/').some(part => part === '..' || part === '.' || part === ''), 'Unsafe checksum path');
    const path = await checkedPath(name);
    const data = await readFile(path);
    assert.equal(digest(data), hash, `Checksum mismatch: ${name}`);
    assert.deepEqual(scanBytes(data, secretsInEnvironment()), [], `Credential pattern in artifact ${name}`);
    const artifact = manifest.artifacts.find(artifact => artifact.path === name);
    if (artifact) { assert.equal(artifact.sha256, hash); assert.equal(artifact.sizeBytes, data.length); }
    if (name.endsWith('.tgz')) inspectPackage(data, secretsInEnvironment());
    seen.push(name);
  }
  assert.deepEqual(seen.sort(), expected, 'Checksums must cover all artifacts and the manifest exactly once');
  // Enumerate artifact directories without the source walker's ignored folders.
  async function entries(path) {
    const files = [];
    for (const entry of await readdir(path, { withFileTypes: true })) {
      assert.ok(!entry.isSymbolicLink(), 'Artifact symlinks are forbidden');
      if (entry.isDirectory()) files.push(...await entries(join(path, entry.name)));
      else files.push(relative(directory, join(path, entry.name)).split(sep).join('/'));
    }
    return files;
  }
  assert.deepEqual((await entries(directory)).sort(), [...expected, 'SHA256SUMS'].sort(), 'Unexpected file in distribution');
  return manifest;
}

export async function buildArtifacts(directory) {
  directory = resolve(directory);
  assert.ok(!await exists(directory), 'Output already exists; verify it or choose a new --out directory');
  const metadata = buildMetadata();
  console.log(`PASS targeted source scan (${await scanSource()} current files; Git history not implied)`);
  const inputs = await buildInputs();
  await mkdir(dirname(directory), { recursive: true });
  const staging = await mkdtemp(join(dirname(directory), '.socketfabric-release-'));
  const temporary = await mkdtemp(join(tmpdir(), 'socketfabric-release-'));
  try {
    const env = { ...cleanEnvironment(), GOWORK: 'off', GOTOOLCHAIN: 'local', CGO_ENABLED: '0', GOFLAGS: '', GOEXPERIMENT: '', GOAMD64: 'v1', GOARM64: 'v8.0' };
    const supported = new Set((await run('go', ['tool', 'dist', 'list'], { env })).stdout.trim().split(/\s+/));
    const goVersion = (await run('go', ['env', 'GOVERSION'], { env })).stdout.trim();
    const host = (await run('go', ['env', 'GOHOSTOS', 'GOHOSTARCH'], { env })).stdout.trim().split(/\s+/).join('/');
    const workspaces = [join(temporary, 'first'), join(temporary, 'second')];
    for (const workspace of workspaces) for (const { path } of inputs.files.filter(({ path }) => /\.go$|go\.(?:mod|sum)$/.test(path))) {
      await mkdir(dirname(join(workspace, path)), { recursive: true });
      await writeFile(join(workspace, path), await readFile(join(root, path)));
    }
    const dependencies = (await run('go', ['list', '-mod=readonly', '-deps', './cmd/socketfabric'], { cwd: workspaces[0], env })).stdout;
    assert.ok(!/socketfabric\/internal|gin-gonic|nats-io|coder\/websocket/.test(dependencies), 'CLI depends on server internals');
    const artifacts = [];
    for (const target of targets) {
      assert.ok(supported.has(target), `Unsupported Go target ${target}`);
      const [GOOS, GOARCH] = target.split('/');
      const path = `cli/socketfabric-${metadata.version}-${GOOS}-${GOARCH}${GOOS === 'windows' ? '.exe' : ''}`;
      await mkdir(dirname(join(staging, path)), { recursive: true });
      const flags = `-s -w -buildid= -X main.version=${metadata.version} -X main.commit=${metadata.commit} -X main.buildTime=${metadata.buildTime}`;
      const destinations = [join(staging, path), join(temporary, 'comparison')];
      for (const [index, workspace] of workspaces.entries()) await run('go', ['build', '-mod=readonly', '-trimpath', '-buildvcs=false', '-ldflags', flags, '-o', destinations[index], './cmd/socketfabric'], { cwd: workspace, env: { ...env, GOOS, GOARCH } });
      const data = await readFile(destinations[0]);
      assert.equal(digest(data), digest(await readFile(destinations[1])), `Non-reproducible CLI ${target}`);
      const info = (await run('go', ['version', '-m', destinations[0]], { env })).stdout;
      assert.ok(info.includes(`GOOS=${GOOS}`) && info.includes(`GOARCH=${GOARCH}`), 'Binary build information differs from target');
      await chmod(destinations[0], 0o755);
      artifacts.push({ path, kind: 'cli', target, sizeBytes: data.length, sha256: digest(data), reproducible: true });
      if (target === host) {
        const info = JSON.parse((await run(destinations[0], ['version', '--json'], { cwd: temporary, env })).stdout);
        for (const name of ['version', 'commit', 'buildTime']) assert.equal(info[name], metadata[name]);
        assert.equal(info.goVersion, goVersion); assert.equal(info.os + '/' + info.arch, target);
        assert.equal(JSON.parse((await run(destinations[0], ['config', 'validate', '--json'], { cwd: temporary, env })).stdout).apiKeyConfigured, false);
        await run(destinations[0], ['--help'], { cwd: temporary, env });
      }
      console.log(`PASS reproducible CLI ${target} (${data.length} bytes)`);
    }
    // Preserve the licenses of code actually linked into the standalone CLI.
    // These notices do not select a license for SocketFabric itself.
    const goroot = (await run('go', ['env', 'GOROOT'], { env })).stdout.trim();
    const jwt = JSON.parse((await run('go', ['list', '-m', '-json', 'github.com/golang-jwt/jwt/v5'], { cwd: workspaces[0], env })).stdout);
    const notices = ['Third-party notices for the SocketFabric CLI. SocketFabric project licensing is still undecided.'];
    for (const [label, source] of [['Go LICENSE', join(goroot, 'LICENSE')], ['Go PATENTS', join(goroot, 'PATENTS')], [`${jwt.Path} ${jwt.Version}`, join(jwt.Dir, 'LICENSE')]]) {
      notices.push(label + '\n\n' + await readFile(source, 'utf8'));
    }
    const notice = Buffer.from(notices.join('\n\n---\n\n') + '\n');
    await writeFile(join(staging, 'THIRD_PARTY_NOTICES.txt'), notice);
    artifacts.push({ path: 'THIRD_PARTY_NOTICES.txt', kind: 'notices', sizeBytes: notice.length, sha256: digest(notice) });
    const sdk = join(root, 'sdk/typescript');
    const dry = JSON.parse((await run(npm, ['pack', '--dry-run', '--json'], { cwd: sdk })).stdout)[0];
    await mkdir(join(staging, 'npm'));
    const packed = JSON.parse((await run(npm, ['pack', '--json', '--pack-destination', join(staging, 'npm')], { cwd: sdk })).stdout)[0];
    assert.deepEqual(dry.files.map(file => file.path).sort(), packed.files.map(file => file.path).sort());
    const second = JSON.parse((await run(npm, ['pack', '--json', '--pack-destination', temporary], { cwd: sdk })).stdout)[0];
    const path = 'npm/' + packed.filename, data = await readFile(join(staging, path));
    assert.equal(digest(data), digest(await readFile(join(temporary, second.filename))), 'Non-reproducible npm tarball');
    const npmPackage = inspectPackage(data, secretsInEnvironment());
    const sourcePackage = JSON.parse(await readFile(join(sdk, 'package.json'), 'utf8'));
    assert.equal(npmPackage.metadata.name, sourcePackage.name); assert.equal(npmPackage.metadata.version, sourcePackage.version);
    assert.equal(npmPackage.metadata.private, sourcePackage.private, 'Packed privacy metadata differs from source');
    artifacts.push({ path, kind: 'npm', sizeBytes: data.length, sha256: digest(data), reproducible: true });
    const manifest = { schemaVersion: 1, publicReleaseApproved: false, metadata, toolchain: { go: goVersion, node: process.version, npm: (await run(npm, ['--version'])).stdout.trim() }, inputs,
      npmPackage: { name: sourcePackage.name, version: sourcePackage.version, private: sourcePackage.private, license: sourcePackage.license ?? null, files: npmPackage.files, unpackedBytes: npmPackage.unpackedBytes }, artifacts };
    await writeFile(join(staging, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n');
    const names = [...artifacts.map(artifact => artifact.path), 'manifest.json'].sort();
    const sums = [];
    for (const name of names) sums.push(`${digest(await readFile(join(staging, name)))}  ${name}`);
    await writeFile(join(staging, 'SHA256SUMS'), sums.join('\n') + '\n');
    await verifyArtifacts(staging);
    assert.equal(inputs.sha256, (await buildInputs()).sha256, 'Build inputs changed during preparation; retry from stable source');
    await rename(staging, directory);
    console.log(`PASS local distribution: ${relative(root, directory)} (6 binaries, npm tarball, manifest, SHA256SUMS); public release remains BLOCKED`);
    return manifest;
  } finally {
    await rm(staging, { recursive: true, force: true });
    await rm(temporary, { recursive: true, force: true });
  }
}

async function main(args) {
  const [command = 'build', ...flags] = args;
  assert.ok(flags.length === 0 || flags.length === 2 && flags[0] === '--out', 'Usage: node scripts/release.mjs [build|verify|scan] [--out directory]');
  const directory = flags[1] ?? join(root, 'bin/release');
  if (command === 'build') await buildArtifacts(directory);
  else if (command === 'verify') { await verifyArtifacts(directory); console.log('PASS artifact checksums, manifest, npm contents and targeted credential scan'); }
  else if (command === 'scan') console.log(`PASS targeted current-source credential scan: ${await scanSource()} files; history NOT VERIFIED without Git`);
  else throw new Error('Unknown release command');
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) main(process.argv.slice(2)).catch(error => { console.error(failureSummary(error)); process.exitCode = 1; });
