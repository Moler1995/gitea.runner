import { createHash, randomUUID } from 'node:crypto';
import { spawn } from 'node:child_process';
import { createReadStream, createWriteStream } from 'node:fs';
import { access, readFile, rename, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { pipeline } from 'node:stream/promises';
import { pathToFileURL } from 'node:url';
import { validateArchive } from './tar.mjs';

const paths = { workspace: '/job/workspace', act: '/job/act', temp: '/job/temp', toolCache: '/job/toolCache' };
const ownerLabel = 'hooks.gitea.io/job';

class HookError extends Error {
  constructor(code, message) { super(message); this.code = code; }
}

function object(value) { return value !== null && typeof value === 'object' && !Array.isArray(value); }

export function configuration(env = process.env) {
  function json(name, fallback, validate) {
    let value = fallback;
    try { if (env[name]) value = JSON.parse(env[name]); } catch { throw new HookError('invalid_config', `${name} must contain valid JSON`); }
    if (!validate(value)) throw new HookError('invalid_config', `${name} has an invalid structure`);
    return value;
  }
  const namespace = env.GITEA_HOOK_NAMESPACE || 'default';
  if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(namespace) || namespace.length > 63) throw new HookError('invalid_config', 'GITEA_HOOK_NAMESPACE must be a Kubernetes namespace name');
  return {
    namespace,
    context: env.GITEA_HOOK_KUBE_CONTEXT,
    nodeSelector: json('GITEA_HOOK_NODE_SELECTOR', {}, value => object(value) && Object.values(value).every(item => typeof item === 'string')),
    tolerations: json('GITEA_HOOK_TOLERATIONS', [], value => Array.isArray(value) && value.every(object)),
    imagePullSecrets: json('GITEA_HOOK_IMAGE_PULL_SECRETS', [], value => Array.isArray(value) && value.every(item => typeof item === 'string' && item.length > 0)),
    securityContext: json('GITEA_HOOK_POD_SECURITY_CONTEXT', { runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000, runAsNonRoot: true, seccompProfile: { type: 'RuntimeDefault' } }, object),
    caFile: env.GITEA_HOOK_CA_CERT_FILE,
  };
}

export function commandArguments(args) {
  if (!args.entryPoint || !path.posix.isAbsolute(args.workingDirectory) || args.user) throw new HookError('unsupported', 'A command needs entryPoint, an absolute workingDirectory, and no user override');
  const env = Object.entries(args.environmentVariables || {}).map(([key, value]) => {
    if (!key || /[=\0]/.test(key) || typeof value !== 'string' || value.includes('\0')) throw new HookError('invalid_request', 'Invalid command environment');
    return `${key}=${value}`;
  });
  return ['/usr/bin/env', ...env, '/bin/sh', '-c', 'cd "$1" && shift && exec "$@"', 'sh', args.workingDirectory, args.entryPoint, ...(args.entryPointArgs || [])];
}

export function commandExit(code, stderr) {
  if (code === 0) return 0;
  const match = stderr.match(/command terminated with (?:non-zero )?exit code[: ]+(\d+)/);
  if (match) return Number(match[1]);
  throw new HookError('kubernetes_error', stderr.trim() || 'Kubernetes exec transport failed');
}

async function processResult(argv, { input, output, live = false, timeout } = {}) {
  const child = spawn('kubectl', argv, { stdio: ['pipe', live ? 'inherit' : 'pipe', 'pipe'], signal: timeout ? AbortSignal.timeout(timeout) : undefined });
  const chunks = [];
  let stderr = '';
  child.stderr.on('data', chunk => {
    stderr = (stderr + chunk.toString()).slice(-16384);
    if (live) process.stderr.write(chunk);
  });
  if (!live && !output) child.stdout.on('data', chunk => chunks.push(chunk));
  const exited = new Promise((resolve, reject) => {
    child.once('error', reject);
    child.once('close', (code, signal) => resolve({ code, signal }));
  });
  const sent = input ? pipeline(input, child.stdin) : Promise.resolve(child.stdin.end());
  const received = output ? pipeline(child.stdout, output) : Promise.resolve();
  const results = await Promise.allSettled([exited, sent, received]);
  if (results[0].status === 'rejected') {
    if (results[0].reason.code === 'ABORT_ERR') throw new HookError('timeout', 'Kubernetes operation exceeded its deadline');
    throw new HookError('kubernetes_error', 'kubectl could not start');
  }
  const status = results[0].value;
  if (status.code === 0 && results.slice(1).some(result => result.status === 'rejected')) throw new HookError('transfer_error', 'Archive stream could not be transferred');
  return { ...status, stdout: Buffer.concat(chunks).toString(), stderr };
}

export async function inClusterConfig(env, directory, serviceAccount = '/var/run/secrets/kubernetes.io/serviceaccount') {
  if (env.KUBECONFIG || !env.KUBERNETES_SERVICE_HOST) return undefined;
  await access(path.join(serviceAccount, 'token'));
  const host = env.KUBERNETES_SERVICE_HOST.includes(':') ? `[${env.KUBERNETES_SERVICE_HOST}]` : env.KUBERNETES_SERVICE_HOST;
  const file = path.join(directory, 'kubeconfig.json');
  const config = {
    apiVersion: 'v1', kind: 'Config',
    clusters: [{ name: 'cluster', cluster: { server: `https://${host}:${env.KUBERNETES_SERVICE_PORT || '443'}`, 'certificate-authority': path.join(serviceAccount, 'ca.crt') } }],
    users: [{ name: 'runner', user: { tokenFile: path.join(serviceAccount, 'token') } }],
    contexts: [{ name: 'runner', context: { cluster: 'cluster', user: 'runner' } }], 'current-context': 'runner',
  };
  await writeFile(file, JSON.stringify(config), { mode: 0o600 });
  return file;
}

class KubernetesHook {
  constructor(request, config, kubeconfig) {
    this.request = request;
    this.config = config;
    this.hash = createHash('sha256').update(request.jobId).digest('hex').slice(0, 24);
    this.pod = `gitea-hook-${this.hash}`;
    this.state = request.state || {};
    if (this.state.pod && this.state.pod !== this.pod || this.state.namespace && this.state.namespace !== config.namespace) throw new HookError('invalid_state', 'State belongs to a different job or namespace');
    this.base = ['--request-timeout=30s', '--namespace', config.namespace];
    if (kubeconfig) this.base.push('--kubeconfig', kubeconfig);
    if (config.context) this.base.push('--context', config.context);
  }

  async result(argv, options) { return processResult([...this.base, ...argv], options); }
  async kube(argv, options) {
    const result = await this.result(argv, options);
    if (result.code !== 0) throw new HookError('kubernetes_error', result.stderr.trim() || 'Kubernetes operation failed');
    return result.stdout;
  }
  async getPod() {
    const text = await this.kube(['get', 'pod', this.pod, '--ignore-not-found=true', '-o', 'json']);
    const pod = text.trim() ? JSON.parse(text) : undefined;
    if (pod && (pod.metadata.labels?.[ownerLabel] !== this.hash || this.state.uid && pod.metadata.uid !== this.state.uid)) throw new HookError('invalid_state', 'Pod ownership does not match this job');
    return pod;
  }
  async exec(argv, options) { return this.result(['exec', '--request-timeout=0', ...(options?.input ? ['-i'] : []), this.pod, '-c', 'job', '--', ...argv], options); }
  async checkedExec(argv, options) {
    const result = await this.exec(argv, options);
    if (result.code !== 0) throw new HookError('kubernetes_error', result.stderr.trim() || 'Job command failed');
    return result.stdout;
  }

  workload(spec) {
    if (!spec?.image || spec.options || spec.volumes?.length || spec.registry && Object.keys(spec.registry).length) throw new HookError('unsupported', 'This demo supports public prebuilt images without options, volumes or registry credentials');
    return { image: spec.image, imagePullPolicy: this.request.args.policy?.forcePull ? 'Always' : 'IfNotPresent', env: Object.entries(spec.environmentVariables || {}).map(([name, value]) => ({ name, value })), securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: ['ALL'] } } };
  }

  jobEnvironment(variables = {}) {
    const defaults = this.config.caFile ? { GIT_SSL_CAINFO: '/job/act/ca-bundle.crt', SSL_CERT_FILE: '/job/act/ca-bundle.crt' } : {};
    return Object.entries({ ...defaults, ...variables }).map(([name, value]) => ({ name, value }));
  }

  async prepare(args) {
    if (args.policy?.privileged || args.policy?.usernsMode || args.policy?.options || args.policy?.capAdd?.length) throw new HookError('unsupported', 'This demo does not implement privileged mode, user namespaces, container options or added capabilities');
    const containers = [{ ...this.workload(args.container), env: this.jobEnvironment(args.container.environmentVariables), name: 'job', command: ['/bin/sh', '-c', 'mkdir -p /job/workspace /job/act /job/temp /job/toolCache /job/home && exec sleep 86400'], volumeMounts: [{ name: 'job', mountPath: '/job' }], resources: { requests: { cpu: '100m', memory: '128Mi' }, limits: { cpu: '2', memory: '1Gi' } } }];
    if (!containers[0].env.some(item => item.name === 'HOME')) containers[0].env.push({ name: 'HOME', value: '/job/home' });
    const services = {};
    const ports = new Set();
    for (const [name, spec] of Object.entries(args.services || {})) {
      if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name) || name.length > 48) throw new HookError('unsupported', 'Service IDs must be DNS labels of at most 48 characters');
      const mapping = {};
      for (const value of spec.ports || []) {
        const match = /^(?:(\d+):)?(\d+)(?:\/tcp)?$/.exec(value);
        if (!match || match[1] && match[1] !== match[2] || Number(match[2]) < 1 || Number(match[2]) > 65535 || ports.has(Number(match[2]))) throw new HookError('unsupported', 'Services need distinct TCP ports without host port remapping');
        mapping[match[2]] = match[2]; ports.add(Number(match[2]));
      }
      const port = Number(Object.keys(mapping)[0]);
      if (!port) throw new HookError('unsupported', 'Declare at least one TCP port for each demo service');
      const container = { ...this.workload(spec), name: `service-${name}`, readinessProbe: { tcpSocket: { port }, periodSeconds: 1 }, resources: { requests: { cpu: '25m', memory: '64Mi' }, limits: { cpu: '500m', memory: '512Mi' } } };
      if (spec.entryPoint?.length) container.command = spec.entryPoint;
      if (spec.entryPointArgs?.length) container.args = spec.entryPointArgs;
      containers.push(container);
      services[name] = { id: `${this.pod}/${container.name}`, ports: mapping };
    }
    const manifest = {
      apiVersion: 'v1', kind: 'Pod',
      metadata: { name: this.pod, labels: { [ownerLabel]: this.hash, 'app.kubernetes.io/name': 'container-hooks-job' } },
      spec: { restartPolicy: 'Never', terminationGracePeriodSeconds: 1, automountServiceAccountToken: false, enableServiceLinks: false,
        nodeSelector: this.config.nodeSelector, tolerations: this.config.tolerations, securityContext: this.config.securityContext,
        imagePullSecrets: this.config.imagePullSecrets.map(name => ({ name })),
        volumes: [{ name: 'job', emptyDir: {} }], containers,
        hostAliases: Object.keys(services).length ? [{ ip: '127.0.0.1', hostnames: Object.keys(services) }] : [],
      },
    };
    const created = JSON.parse(await this.kube(['create', '-f', '-', '-o', 'json'], { input: [Buffer.from(JSON.stringify(manifest))] }));
    this.state = { pod: this.pod, namespace: this.config.namespace, uid: created.metadata.uid, forcePull: Boolean(args.policy?.forcePull) };
    console.log(`Created job Pod ${this.pod}`);
    await this.kube(['wait', '--for=condition=Ready', `pod/${this.pod}`, '--timeout=180s', '--request-timeout=190s']);
    const imageEnvironment = Object.fromEntries((await this.checkedExec(['/usr/bin/env', '-0'])).split('\0').filter(Boolean).map(value => { const index = value.indexOf('='); return [value.slice(0, index), value.slice(index + 1)]; }));
    if (this.config.caFile) {
      const certificate = await readFile(this.config.caFile);
      if (!certificate.includes('-----BEGIN CERTIFICATE-----') || certificate.includes('PRIVATE KEY')) throw new HookError('invalid_config', 'GITEA_HOOK_CA_CERT_FILE must contain public PEM certificates');
      await this.checkedExec(['/bin/sh', '-c', 'if [ -f /etc/ssl/certs/ca-certificates.crt ]; then cat /etc/ssl/certs/ca-certificates.crt > /job/act/ca-bundle.crt; fi; cat >> /job/act/ca-bundle.crt'], { input: [certificate] });
    }
    const arch = (await this.checkedExec(['uname', '-m'])).trim();
    const architecture = { x86_64: 'X64', aarch64: 'ARM64' }[arch];
    if (!architecture) throw new HookError('unsupported', `Unsupported job architecture: ${arch}`);
    return { paths, runner: { os: 'Linux', arch: architecture }, imageEnvironment, container: { id: `${this.pod}/job`, network: this.pod }, services };
  }

  async dispatch() {
    const { command, args } = this.request;
    if (command === 'prepare_job') return this.prepare(args);
    const pod = await this.getPod();
    if (command === 'cleanup_job') {
      if (pod) await this.kube(['delete', 'pod', this.pod, '--wait=true', '--timeout=30s']);
      console.log(`Cleaned job Pod ${this.pod}`);
      return {};
    }
    if (!pod) throw new HookError('job_unavailable', 'Job Pod is absent');
    if (command === 'run_command_step') {
      const result = await this.exec(commandArguments(args), { live: true });
      return { exitCode: commandExit(result.code, result.stderr) };
    }
    if (command === 'copy_to_job') {
      if (!path.posix.isAbsolute(args.destination)) throw new HookError('invalid_request', 'Copy destination must be absolute');
      await validateArchive(args.archiveFile);
      await this.checkedExec(['mkdir', '-p', args.destination]);
      await this.checkedExec(['tar', '-xpf', '-', '-C', args.destination], { input: createReadStream(args.archiveFile) });
      return {};
    }
    if (command === 'copy_from_job') {
      if (!path.posix.isAbsolute(args.source)) throw new HookError('invalid_request', 'Copy source must be absolute');
      const exists = await this.exec(['/bin/sh', '-c', 'test -e "$1" || test -L "$1"', 'sh', args.source]);
      if (commandExit(exists.code, exists.stderr) === 1) throw new HookError('not_found', 'Job source is absent');
      await this.checkedExec(['tar', '-cf', '-', '-C', path.posix.dirname(args.source), path.posix.basename(args.source)], { output: createWriteStream(args.archiveFile, { mode: 0o600 }) });
      return {};
    }
    if (command === 'run_container_step') {
      if (args.dockerfile || args.buildContextArchive || args.user || !args.image) throw new HookError('unsupported', 'This demo runs prebuilt container actions without a user override');
      const name = `step-${createHash('sha256').update(this.request.requestId).digest('hex').slice(0, 16)}`;
      const step = { name, image: args.image, imagePullPolicy: this.state.forcePull ? 'Always' : 'IfNotPresent', workingDir: args.workingDirectory,
        env: this.jobEnvironment(args.environmentVariables), volumeMounts: [{ name: 'job', mountPath: '/job' }],
        securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: ['ALL'] } },
      };
      if (args.entryPoint) step.command = [args.entryPoint];
      if (args.entryPointArgs?.length) step.args = args.entryPointArgs;
      await this.kube(['patch', 'pod', this.pod, '--subresource=ephemeralcontainers', '--type=merge', '-p', JSON.stringify({ spec: { ephemeralContainers: [...(pod.spec.ephemeralContainers || []), step] } })]);
      const deadline = Date.now() + 180000;
      let streamed = false;
      while (Date.now() < deadline) {
        const current = await this.getPod();
        const status = current?.status.ephemeralContainerStatuses?.find(status => status.name === name);
        if (!streamed && (status?.state.running || status?.state.terminated)) {
          await this.kube(['logs', '-f', this.pod, '-c', name, '--request-timeout=0'], { live: true, timeout: Math.max(1, deadline - Date.now()) });
          streamed = true;
        }
        if (streamed && status?.state.terminated) return { exitCode: status.state.terminated.exitCode };
        await new Promise(resolve => setTimeout(resolve, 500));
      }
      throw new HookError('timeout', 'Container action did not complete within 180 seconds');
    }
    throw new HookError('unsupported', `Unknown command: ${command}`);
  }
}

export async function main() {
  let data = '';
  for await (const chunk of process.stdin) { data += chunk; if (Buffer.byteLength(data) > 8 << 20) throw new Error('Hook request exceeds 8 MiB'); }
  const request = JSON.parse(data);
  if (request.protocolVersion !== 1 || !request.jobId || !request.requestId || !path.isAbsolute(request.responseFile) || !object(request.args) || !object(request.state)) throw new Error('Invalid hook protocol envelope');
  let backend;
  const response = { protocolVersion: 1, requestId: request.requestId, state: request.state, result: {} };
  try {
    const config = configuration();
    const kubeconfig = await inClusterConfig(process.env, path.dirname(request.responseFile));
    if (kubeconfig) {
      const direct = [process.env.NO_PROXY || process.env.no_proxy, process.env.KUBERNETES_SERVICE_HOST].filter(Boolean).join(',');
      process.env.NO_PROXY = process.env.no_proxy = direct;
    }
    backend = new KubernetesHook(request, config, kubeconfig);
    response.result = await backend.dispatch();
  } catch (error) {
    response.error = { code: error.code || 'hook_error', message: error.message };
  }
  response.state = backend?.state || request.state;
  const temporary = `${request.responseFile}.${randomUUID()}`;
  await writeFile(temporary, JSON.stringify(response), { mode: 0o600 });
  await rename(temporary, request.responseFile);
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  main().catch(error => { console.error(error.message); process.exitCode = 1; });
}
