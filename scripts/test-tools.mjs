// Pinned local test tools only; no project dependency or external publication.
import assert from 'node:assert/strict';
import { access, mkdir } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { cleanEnvironment, root, run } from './release.mjs';

export const natsVersion = '2.15.1';
const directory = join(root, '.cache/test-tools');
const executable = name => name + (process.platform === 'win32' ? '.exe' : '');

export async function resolveNatsServer(environment = process.env) {
  const candidates = environment.NATS_SERVER_BIN ? [environment.NATS_SERVER_BIN] : [join(directory, executable('nats-server')), 'nats-server'];
  for (const candidate of candidates) {
    let result;
    try { result = await run(candidate, ['-v'], { allowFailure: true }); } catch { continue; }
    if (result.code !== 0) continue;
    assert.equal(result.stdout.trim(), 'nats-server: v' + natsVersion, 'Integration NATS server version differs from the pinned Docker/test version');
    return candidate;
  }
  throw new Error('Pinned NATS server unavailable. Run make test-tools or set NATS_SERVER_BIN to nats-server v' + natsVersion);
}

export async function resolveVulnerabilityScanner() {
  if (process.env.GOVULNCHECK_BIN) return process.env.GOVULNCHECK_BIN;
  const candidate = join(directory, executable('govulncheck'));
  return await access(candidate).then(() => candidate, () => undefined);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    if (process.argv.includes('--install')) {
      await mkdir(directory, { recursive: true });
      const env = { ...cleanEnvironment(), GOBIN: directory, GOWORK: 'off', GOTOOLCHAIN: 'local' };
      for (const tool of ['github.com/nats-io/nats-server/v2@v' + natsVersion, 'golang.org/x/vuln/cmd/govulncheck@v1.9.0']) {
        await run('go', ['install', tool], { env, timeout: 240_000 });
      }
    }
    console.log('NATS_SERVER_BIN=' + await resolveNatsServer());
    const scanner = await resolveVulnerabilityScanner();
    if (scanner) console.log('GOVULNCHECK_BIN=' + scanner);
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
