import { build } from 'esbuild';
import { copyFile, mkdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

await mkdir(new URL('./public/', import.meta.url), { recursive: true });
await build({
  absWorkingDir: fileURLToPath(new URL('.', import.meta.url)),
  entryPoints: ['app.ts'],
  outfile: 'public/app.js',
  bundle: true,
  platform: 'browser',
  format: 'esm',
  target: 'es2022',
  sourcemap: false,
});
await copyFile(new URL('./index.html', import.meta.url), new URL('./public/index.html', import.meta.url));
