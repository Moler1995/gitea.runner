# Jobs as Kubernetes pods

A `kubernetes` label runs each job as a pod, without a Docker daemon:

```text
ubuntu-latest:kubernetes://docker.gitea.com/runner-images:ubuntu-latest
```

The pod has a `job` container from the label's image or the job's `container.image`, plus one container per service.

## Setup

In a cluster, the runner uses its pod's service account and namespace. [`examples/kubernetes/kubernetes-pods.yaml`](../examples/kubernetes/kubernetes-pods.yaml) deploys a runner with these permissions:

| Resource | Verbs |
| --- | --- |
| `pods` | `create`, `get`, `list`, `delete` |
| `pods/exec` | `create`, `get` |
| `pods/log` | `get` |
| `secrets` | `create`, `delete` |

Outside a cluster, it uses the current context of the first file in `$KUBECONFIG`, or of `~/.kube/config`, which must authenticate with a token, token file or client certificate. The cluster must run Kubernetes 1.30 or later, and job images need `sh`, `tar`, `tail` and `env`.

## Configuration

```yaml
kubernetes:
  kubeconfig: "" # instead of the service account
  namespace: "" # defaults to the service account's, the kubeconfig context's, or default
  pod_template: # merged under each job pod, a container named job merges into the job container, one named $services into each service container
    spec:
      containers:
        - name: job
          resources:
            limits:
              memory: 4Gi
        - name: $services
          resources:
            limits:
              memory: 1Gi
  pod_templates: # merged over pod_template for jobs whose runs-on has the label they are keyed by
    gpu:
      spec:
        nodeSelector:
          nvidia.com/gpu.present: "true"
```

A job with `runs-on: [linux, gpu]` gets the `linux` and then the `gpu` entry of `pod_templates` merged over `pod_template`, while its image comes from the first of its labels the runner has. A template volume named `workspace` or `act` replaces the `emptyDir` the runner mounts at the workspace or at `/var/run/act`, for example to limit its size. Images are pulled by the cluster's `imagePullPolicy` default, a template's, or always with `container.force_pull`. Like every config value, templates expand `${NAME}` from the runner's environment, so a shell command in a template writes `$${NAME}` for the pod's own variable.

Job pods mount no service account token unless the template sets `automountServiceAccountToken: true`. Job pods and their Secrets are labeled `app.kubernetes.io/managed-by: gitea-runner` and `com.gitea.runner.uuid: <runner UUID>`. The idle cleanup removes those the runner did not get to remove, once they are older than `runner.workdir_cleanup_age`.

## Copying files over the pod network

The runner copies files into the job container, such as the actions a step uses, with `tar` over `pods/exec`. That data goes through the API server, which on a managed cluster sits in other zones than the nodes and is billed as cross-zone traffic, twice per byte. With `transfer.mode` set to `auto` or `direct` the job container fetches the files from the runner pod instead:

```yaml
kubernetes:
  transfer:
    mode: direct # api (default), auto or direct
    host: "" # address job pods reach the runner at, defaults to its outbound IP, the pod IP in a cluster
    port: 0 # port it listens on, defaults to a free one
    image: "" # image holding the runner binary at the runner's own path, defaults to the runner pod's image if it has one container
```

Each job pod then gets an init container from `image` that copies the runner binary into the `act` volume, and every copy is fetched with it from a one-time URL on the runner over plain HTTP. The modes differ in what happens when that does not work, for example when a NetworkPolicy keeps job pods from reaching the runner, or the runner pod has several containers and no `image` is set:

| Mode | Copies | When the pod network does not work |
|---|---|---|
| `api` | through the API server | not used |
| `auto` | over the pod network | the job pod gets its copies through the API server, after one failed attempt, and a warning is logged |
| `direct` | over the pod network | the job fails, so a broken setup shows instead of quietly costing API server traffic again |

 Copies out of the job container and step output still go through the API server. Job pods need to be able to pull `image`, so a private one needs `imagePullSecrets` in `pod_template`. The init container is named `gitea-runner-transfer`, an `initContainers` entry of that name in `pod_template` merges into it, for example to set its `securityContext` or `resources`.

## Services

Services share the pod's network, so a step reaches one by its id or `localhost`, and two services cannot listen on the same port. An id that is not a valid hostname is sanitized, so `my_db` is reached as `my-db`. Pass secrets to services through `env`, as `command` is visible in the pod spec. Of `options`, only `--health-*` is supported. They become a startup probe with Docker's defaults, which stops a service that fails its retries, so the job fails as on Docker.

## Unsupported

Docker actions, `docker://` steps, and the `options`, `volumes` and `credentials` of the job container fail the job, as do `volumes` and `credentials` of services. Run such workflows on a [Docker in Docker](../examples/kubernetes/README.md) runner. Pull private images with `imagePullSecrets` in `pod_template`.
