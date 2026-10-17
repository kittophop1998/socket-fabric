import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtemp, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { gzipSync } from 'node:zlib';
import { buildMetadata, cleanEnvironment, digest, failureSummary, inspectPackage, scanBytes, secretsInEnvironment, verifyArtifacts } from './release.mjs';

test('build subprocesses omit application/registry secrets and errors omit JSON contents', () => {
  assert.deepEqual(cleanEnvironment({ PATH: '/tools', NATS_SERVER_BIN: '/nats', CLIENT_TOKEN_SECRET: 'hidden', NPM_TOKEN: 'hidden', SOCKETFABRIC_API_KEY: 'hidden', PUBLISHER_KEYS_JSON: '{}', EXAMPLE_USERS_JSON: '{}' }), { PATH: '/tools', NATS_SERVER_BIN: '/nats' });
  assert.ok(!failureSummary(new SyntaxError('invalid-private-test-input')).includes('private-test-input'));
  assert.equal(failureSummary(new Error('Fixed error\nunsafe detail')), 'Fixed error');
});

test('metadata is deterministic and rejects shell/ldflag/path injection', () => {
  assert.deepEqual(buildMetadata({}), { version: 'dev', commit: 'unknown', buildTime: '1970-01-01T00:00:00Z', sourceDateEpoch: 0 });
  assert.equal(buildMetadata({ SOURCE_DATE_EPOCH: '1767225600' }).buildTime, '2026-01-01T00:00:00Z');
  assert.equal(buildMetadata({ VERSION: '0.1.0-rc.1', COMMIT: 'a'.repeat(40), BUILD_TIME: '2026-10-16T00:00:00Z' }).version, '0.1.0-rc.1');
  for (const environment of [{ VERSION: '../escape' }, { VERSION: 'dev -X main.commit=changed' }, { COMMIT: 'x; echo' }, { SOURCE_DATE_EPOCH: '-1' }, { SOURCE_DATE_EPOCH: '999999999999' }, { BUILD_TIME: '2026-02-31T00:00:00Z' }]) {
    assert.throws(() => buildMetadata(environment));
  }
});

test('targeted scans identify credential material without returning it', () => {
  const secret = 'test-only-' + 'x'.repeat(40);
  for (const value of [secret, Buffer.from(secret).toString('base64'), encodeURIComponent(secret)]) {
    assert.deepEqual(scanBytes(Buffer.from(value), [secret]), ['configured-credential']);
  }
  const values = [
    ['ghp_' + 'a'.repeat(36), 'github-token'], ['npm_' + 'b'.repeat(36), 'npm-token'],
    ['AKIA' + 'A'.repeat(16), 'aws-access-key'],
    ['-----BEGIN ' + 'PRIVATE KEY-----\n' + 'A'.repeat(64) + '\n', 'private-key'],
    ['eyJ' + 'a'.repeat(20) + '.eyJ' + 'b'.repeat(20) + '.' + 'c'.repeat(40), 'literal-jwt'],
  ];
  for (const [value, label] of values) {
    const result = scanBytes(value);
    assert.ok(result.includes(label)); assert.ok(!JSON.stringify(result).includes(value));
  }
  assert.deepEqual(scanBytes('CLIENT_TOKEN_SECRET=\nPUBLISHER_KEYS_JSON={}'), []);
  assert.deepEqual(secretsInEnvironment({ PUBLISHER_KEYS_JSON: JSON.stringify({ p: secret }) }), [secret]);
  const databaseURL = 'postgres://operator:' + secret + '@localhost/socketfabric';
  assert.ok(secretsInEnvironment({ DATABASE_URL: databaseURL }).includes(secret));
  assert.deepEqual(cleanEnvironment({ PATH: '/tools', DATABASE_URL: databaseURL, MANAGEMENT_ENABLED: 'true', POSTGRES_USER: 'operator' }), { PATH: '/tools' });
  assert.deepEqual(scanBytes('sfpk_' + 'A'.repeat(26) + '.' + 'b'.repeat(43)), ['management-credential']);
});

// A small tar fixture tests malicious archive rejection, not npm's implementation.
function tarball(path, data = Buffer.from('{}'), type = '0') {
  const header = Buffer.alloc(512);
  header.write(path, 0, 100);
  header.write('0000644\0', 100); header.write('0000000\0', 108); header.write('0000000\0', 116);
  header.write(data.length.toString(8).padStart(11, '0') + '\0', 124);
  header.write('00000000000\0', 136); header.fill(32, 148, 156); header.write(type, 156);
  header.write('ustar\0', 257);
  const checksum = header.reduce((sum, byte) => sum + byte, 0);
  header.write(checksum.toString(8).padStart(6, '0') + '\0 ', 148);
  return gzipSync(Buffer.concat([header, data, Buffer.alloc((512 - data.length % 512) % 512), Buffer.alloc(1024)]));
}

test('npm inspection rejects traversal, links, secrets, corruption and decompression overflow', () => {
  for (const name of ['package/../.env', '/absolute', 'package/test/example.js', 'package/.npmrc']) {
    assert.throws(() => inspectPackage(tarball(name)), /Unexpected npm/);
  }
  assert.throws(() => inspectPackage(tarball('package/package.json', Buffer.from('{}'), '2')), /regular npm/);
  const credential = 'npm_' + 'b'.repeat(36);
  assert.throws(() => inspectPackage(tarball('package/README.md', Buffer.from(credential))), /Credential pattern/);
  assert.throws(() => inspectPackage(Buffer.from('invalid gzip')));
  assert.throws(() => inspectPackage(gzipSync(Buffer.alloc(9 << 20))));
});

test('checksum verification rejects tampering, duplicate coverage, extras and traversal', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'socketfabric-checksum-test-'));
  const artifact = Buffer.from('test artifact');
  const manifest = { schemaVersion: 1, publicReleaseApproved: false, artifacts: [{ path: 'artifact', sha256: digest(artifact), sizeBytes: artifact.length }] };
  try {
    await writeFile(join(directory, 'artifact'), artifact);
    await writeFile(join(directory, 'manifest.json'), JSON.stringify(manifest));
    const manifestHash = digest(await readFile(join(directory, 'manifest.json')));
    const valid = `${digest(artifact)}  artifact\n${manifestHash}  manifest.json\n`;
    await writeFile(join(directory, 'SHA256SUMS'), valid);
    await verifyArtifacts(directory);
    await writeFile(join(directory, 'artifact'), 'tampered');
    await assert.rejects(verifyArtifacts(directory), /Checksum mismatch/);
    await writeFile(join(directory, 'artifact'), artifact);
    await writeFile(join(directory, 'SHA256SUMS'), valid + valid.split('\n')[0] + '\n');
    await assert.rejects(verifyArtifacts(directory), /exactly once/);
    await writeFile(join(directory, 'SHA256SUMS'), valid);
    await writeFile(join(directory, 'unexpected'), 'extra');
    await assert.rejects(verifyArtifacts(directory), /Unexpected file/);
    await rm(join(directory, 'unexpected'));
    await writeFile(join(directory, 'SHA256SUMS'), `${digest(artifact)}  ../escape\n`);
    await assert.rejects(verifyArtifacts(directory), /Unsafe checksum path/);
    await writeFile(join(directory, 'SHA256SUMS'), valid);
    await rm(join(directory, 'manifest.json'));
    await symlink(join(directory, 'artifact'), join(directory, 'manifest.json'));
    await assert.rejects(verifyArtifacts(directory), /symlinks are forbidden/);
    await rm(join(directory, 'manifest.json'));
    await writeFile(join(directory, 'manifest.json'), 'not-json-sensitive-test-input');
    await assert.rejects(verifyArtifacts(directory), error => error.message.includes('Invalid artifact manifest JSON') && !error.message.includes('sensitive-test-input'));
  } finally { await rm(directory, { recursive: true, force: true }); }
});
