import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { spawn, execFile } from 'node:child_process';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { promisify } from 'node:util';

const exec = promisify(execFile);
const root = await mkdtemp(path.join(os.tmpdir(), 'hook-smoke-'));
const jobId = randomUUID();
let state = {};

async function invoke(command, args, expectedError) {
  const responseFile = path.join(root, `${randomUUID()}.json`);
  const request = { protocolVersion: 1, requestId: randomUUID(), jobId, command, responseFile, state, args };
  const child = spawn(process.execPath, [new URL('./index.mjs', import.meta.url).pathname], { stdio: ['pipe', 'inherit', 'inherit'] });
  child.stdin.end(JSON.stringify(request));
  await new Promise((resolve, reject) => { child.once('error', reject); child.once('close', code => code ? reject(new Error(`Hook process exited ${code}`)) : resolve()); });
  const response = JSON.parse(await readFile(responseFile, 'utf8'));
  assert.equal(response.requestId, request.requestId);
  state = response.state;
  if (expectedError) assert.equal(response.error?.code, expectedError);
  else assert.equal(response.error, undefined, JSON.stringify(response.error));
  console.log(`${command}: ${expectedError || 'OK'}`);
  return response.result;
}

try {
  const prepared = await invoke('prepare_job', {
    container: { image: process.env.GITEA_HOOK_SMOKE_IMAGE || 'node:24-bookworm' }, policy: {},
    services: { web: { image: 'traefik/whoami:v1.11.0', environmentVariables: { WHOAMI_PORT_NUMBER: '18080' }, ports: ['18080'] } },
  });
  const input = path.join(root, 'files');
  await mkdir(input);
  await writeFile(path.join(input, 'input.txt'), 'archive transported\n');
  const archiveFile = path.join(root, 'to.tar');
  await exec('tar', ['-cf', archiveFile, '-C', input, '.']);
  await invoke('copy_to_job', { archiveFile, destination: prepared.paths.workspace });
  const expectedCA = { EXPECTED_GIT_SSL_CAINFO: prepared.imageEnvironment.GIT_SSL_CAINFO || '', EXPECTED_SSL_CERT_FILE: prepared.imageEnvironment.SSL_CERT_FILE || '' };
  if (process.env.GITEA_HOOK_CA_CERT_FILE) assert.ok(expectedCA.EXPECTED_GIT_SSL_CAINFO);
  const checkCA = 'test "${GIT_SSL_CAINFO-}" = "$EXPECTED_GIT_SSL_CAINFO" && test "${SSL_CERT_FILE-}" = "$EXPECTED_SSL_CERT_FILE" && if [ -n "${GIT_SSL_CAINFO-}" ]; then test -s "$GIT_SSL_CAINFO"; fi';
  const command = { entryPoint: '/bin/sh', entryPointArgs: ['-c', checkCA + ' && test "$(cat input.txt)" = "archive transported" && test "$VALUE" = \'literal value $HOME\' && curl -fsS http://web:18080/health >/dev/null && echo COMMAND_ENV_AND_SERVICE_OK'], workingDirectory: prepared.paths.workspace, environmentVariables: { VALUE: 'literal value $HOME', ...expectedCA } };
  assert.equal((await invoke('run_command_step', command)).exitCode, 0);
  assert.equal((await invoke('run_command_step', { ...command, entryPointArgs: ['-c', 'exit 7'] })).exitCode, 7);
  assert.equal((await invoke('run_container_step', { image: 'busybox:1.37', entryPoint: '/bin/sh', entryPointArgs: ['-c', checkCA + ' && cat input.txt > from-container.txt && echo CONTAINER_STEP_OK'], workingDirectory: prepared.paths.workspace, environmentVariables: expectedCA })).exitCode, 0);
  const from = path.join(root, 'from.tar');
  await invoke('copy_from_job', { source: `${prepared.paths.workspace}/from-container.txt`, archiveFile: from });
  assert.equal((await exec('tar', ['-xOf', from, 'from-container.txt'])).stdout, 'archive transported\n');
  await invoke('copy_from_job', { source: `${prepared.paths.workspace}/absent.txt`, archiveFile: from }, 'not_found');
} finally {
  try { await invoke('cleanup_job', {}); await invoke('cleanup_job', {}); await invoke('copy_from_job', { source: '/job/act/output.txt', archiveFile: path.join(root, 'after-cleanup.tar') }, 'job_unavailable'); }
  finally { await rm(root, { recursive: true, force: true }); }
}
