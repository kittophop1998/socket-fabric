// Node >=24.3 supplies the TypeScript stripper; no npm dependencies are needed.
import { readFile, writeFile } from 'node:fs/promises';
import { stripTypeScriptTypes } from 'node:module';

const source = new URL('./demo.ts', import.meta.url);
const output = new URL('./demo.js', import.meta.url);
const javascript = '// Generated from demo.ts by demo-build.mjs; do not edit.\n' +
  stripTypeScriptTypes(await readFile(source, 'utf8'), { mode: 'strip' });
if (process.argv.includes('--check')) {
  if (await readFile(output, 'utf8') !== javascript) throw new Error('demo.js is stale; run demo-build.mjs');
} else {
  await writeFile(output, javascript);
}
