import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { promisify } from 'node:util';
import test from 'node:test';
import { commandArguments, commandExit, configuration, inClusterConfig } from './index.mjs';
import { validateArchive } from './tar.mjs';

const exec = promisify(execFile);

test('generic scheduling defaults and explicit overrides', () => {
  assert.deepEqual(configuration({}).nodeSelector, {});
  assert.deepEqual(configuration({}).tolerations, []);
  assert.equal(configuration({}).namespace, 'default');
  const config = configuration({ GITEA_HOOK_NAMESPACE: 'my-jobs', GITEA_HOOK_NODE_SELECTOR: '{"disktype":"ssd"}', GITEA_HOOK_TOLERATIONS: '[{"key":"custom","operator":"Exists"}]' });
  assert.deepEqual(config.nodeSelector, { disktype: 'ssd' });
  assert.equal(config.tolerations[0].key, 'custom');
  assert.throws(() => configuration({ GITEA_HOOK_NODE_SELECTOR: '[]' }), /invalid structure/);
});

test('exec preserves argv, environment and working directory without shell interpolation', { skip: process.platform === 'win32' }, async t => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'hook-exec-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const directory = path.join(root, "space ' quoted");
  await mkdir(directory);
  const value = 'literal $HOME; $(touch should-not-exist) "quote"\nnewline';
  const argv = commandArguments({ entryPoint: process.execPath, entryPointArgs: ['-e', 'console.log(JSON.stringify({cwd:process.cwd(),value:process.env.VALUE,arg:process.argv[1]}))', value], environmentVariables: { VALUE: value }, workingDirectory: directory });
  const result = JSON.parse((await exec(argv[0], argv.slice(1))).stdout);
  assert.equal(result.cwd, directory);
  assert.equal(result.value, value);
  assert.equal(result.arg, value);
  await assert.rejects(readFile(path.join(directory, 'should-not-exist')), { code: 'ENOENT' });
  assert.equal(commandExit(1, 'command terminated with exit code 7'), 7);
  assert.throws(() => commandExit(1, 'connection refused'), /connection refused/);
});

test('in-cluster config references projected files rather than copying token values', async t => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'hook-sa-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const account = path.join(root, 'sa');
  await mkdir(account);
  await writeFile(path.join(account, 'token'), 'token-must-not-be-copied');
  const filename = await inClusterConfig({ KUBERNETES_SERVICE_HOST: 'fd00::1', KUBERNETES_SERVICE_PORT: '443' }, root, account);
  const contents = await readFile(filename, 'utf8');
  const config = JSON.parse(contents);
  assert.equal(config.clusters[0].cluster.server, 'https://[fd00::1]:443');
  assert.equal(config.users[0].user.tokenFile, path.join(account, 'token'));
  assert.ok(!contents.includes('token-must-not-be-copied'));
  assert.equal(await inClusterConfig({ KUBECONFIG: '/custom/config', KUBERNETES_SERVICE_HOST: '127.0.0.1' }, root, account), undefined);
});

function archive(name, target, type = '0') {
  const data = Buffer.alloc(1536);
  data.write(name, 0, 100);
  data.write('00000000000', 124, 11);
  data.write(type, 156);
  if (target) data.write(target, 157, 100);
  return data;
}

test('tar transport accepts relative entries and rejects path/link escapes', async t => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'hook-tar-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  for (const [index, data, valid] of [
    [0, archive('dir/file'), true],
    [1, archive('../outside'), false],
    [2, archive('/outside'), false],
    [3, archive('dir/link', '../file', '2'), true],
    [4, archive('dir/link', '../../outside', '2'), false],
    [5, archive('link', '/etc/passwd', '2'), false],
    [6, archive('hard', '../outside', '1'), false],
  ]) {
    const filename = path.join(root, `${index}.tar`);
    await writeFile(filename, data);
    if (valid) await validateArchive(filename);
    else await assert.rejects(validateArchive(filename));
  }
});
