import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { Parser, fromFile } from '@asyncapi/parser';
import openapiTS, { astToString } from 'openapi-typescript';
import ts from 'typescript';

const directory = dirname(fileURLToPath(import.meta.url));
const root = resolve(directory, '../..');
const temporary = await mkdtemp(join(tmpdir(), 'socketfabric-contracts-'));
try {
  const openapiPath = join(root, 'api/openapi.yaml');
  const asyncapiPath = join(root, 'api/asyncapi.yaml');
  const openapi = astToString(await openapiTS(pathToFileURL(openapiPath), { cwd: pathToFileURL(root) }));
  const generated = join(temporary, 'openapi.ts');
  await writeFile(generated, openapi);

  const parser = new Parser();
  const parsed = await fromFile(parser, asyncapiPath).parse();
  const errors = parsed.diagnostics.filter(diagnostic => diagnostic.severity === 0);
  assert.equal(errors.length, 0, 'AsyncAPI parser reported errors: ' + errors.map(error => error.message).join('; '));
  assert.ok(parsed.document, 'AsyncAPI document missing');

  const consumer = join(temporary, 'consumer.ts');
  await writeFile(consumer, `import type { components, paths } from './openapi.js';\nimport type { EventEnvelope } from ${JSON.stringify(join(root, 'sdk/typescript/src/index.js'))};\nconst project: components['schemas']['Project'] | undefined = undefined;\nconst publish: paths['/v1/events']['post'] | undefined = undefined;\nconst event: EventEnvelope | undefined = undefined;\nvoid [project, publish, event];\n`);
  const tsconfig = join(temporary, 'tsconfig.json');
  await writeFile(tsconfig, JSON.stringify({
    compilerOptions: { strict: true, noEmit: true, target: 'ES2022', module: 'NodeNext', moduleResolution: 'NodeNext', skipLibCheck: true },
    files: [generated, consumer],
  }));
  const config = ts.readConfigFile(tsconfig, ts.sys.readFile);
  assert.equal(config.error, undefined, 'Temporary TypeScript config is invalid');
  const parsedConfig = ts.parseJsonConfigFileContent(config.config, ts.sys, temporary);
  const program = ts.createProgram(parsedConfig.fileNames, parsedConfig.options);
  const diagnostics = ts.getPreEmitDiagnostics(program);
  assert.equal(diagnostics.length, 0, 'Generated OpenAPI TypeScript consumer failed typecheck: ' + ts.formatDiagnosticsWithColorAndContext(diagnostics, {
    getCurrentDirectory: ts.sys.getCurrentDirectory,
    getCanonicalFileName: file => file,
    getNewLine: () => ts.sys.newLine,
  }));
  console.log('PASS OpenAPI bundle/type generation, AsyncAPI parse, strict TypeScript consumer');
} finally {
  await rm(temporary, { recursive: true, force: true });
}
