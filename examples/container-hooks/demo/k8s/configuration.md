# Container hooks (Kubernetes API demo): configuration

The backend uses Node.js 24 and kubectl. The job and services run in one Pod; container actions share its emptyDir. Files travel as tar archives over Kubernetes exec. Configure the hook process through environment variables.

| Variable | Default | Meaning |
| --- | --- | --- |
| `GITEA_HOOK_NAMESPACE` | `default` | Namespace for job Pods. The quickstart explicitly selects `container-hooks-demo`. |
| `GITEA_HOOK_KUBE_CONTEXT` | kubeconfig's current context | Context for an external kubeconfig. |
| `GITEA_HOOK_NODE_SELECTOR` | `{}` | JSON nodeSelector, e.g. `{"disktype":"ssd"}`. |
| `GITEA_HOOK_TOLERATIONS` | `[]` | JSON tolerations, e.g. `[{"key":"dedicated","operator":"Equal","value":"ci","effect":"NoSchedule"}]`. |
| `GITEA_HOOK_IMAGE_PULL_SECRETS` | `[]` | JSON names of existing imagePullSecrets in the job namespace. |
| `GITEA_HOOK_POD_SECURITY_CONTEXT` | UID/GID/fsGroup 1000, non-root | JSON Pod securityContext. |
| `GITEA_HOOK_CA_CERT_FILE` | unset | Public PEM CA file on the runner for the job's Git/TLS clients. |
| `KUBECONFIG` | kubectl defaults or ServiceAccount | Kubernetes API credentials. They are not passed to job Pods. |

For Docker, add settings to `.tmp/container-hooks-demo/runner.env`. For the in-kind option, update the `runner-env` Secret and restart the Deployment. Its `GITEA_HOOK_NAMESPACE` is set separately through the Downward API to the Deployment's namespace.

For Gitea with a private CA, set `runner.ca_file` in a mounted runner configuration and select it with `CONFIG_FILE`. `GITEA_HOOK_CA_CERT_FILE` adds the CA to the job certificate bundle. The hook sets `GIT_SSL_CAINFO` and `SSL_CERT_FILE` on the job container and container actions; workflow values take precedence. The runner inherits proxy variables and supplies them to commands and services through its existing proxy handling.

## Check the protocol without Gitea

After creating kind and building the image:

```sh
kind get kubeconfig --name container-hooks-demo --internal > .tmp/container-hooks-demo/docker-kubeconfig
docker run --rm --network kind \
  -e KUBECONFIG=/config/kubeconfig \
  -e GITEA_HOOK_NAMESPACE=container-hooks-demo \
  --mount type=bind,src="$PWD/.tmp/container-hooks-demo/docker-kubeconfig",dst=/config/kubeconfig,readonly \
  --entrypoint node gitea-runner:container-hooks-k8s-demo /opt/container-hooks/k8s/smoke.mjs
```

This checks all six commands, file transfer, a service, a nonzero exit code, a container step and repeated cleanup.

Check the active runner configuration inside kind, including its configured CA:

```sh
kubectl -n container-hooks-demo exec deployment/container-hooks-demo -- node /opt/container-hooks/k8s/smoke.mjs
```

## Demo limitations

The job image must contain `/bin/sh`, `/usr/bin/env`, tar and the workflow's tools. Services must declare distinct TCP ports; readiness checks the first port. Service names resolve to the Pod's loopback address. Container actions use ephemeral containers with prebuilt images.

The hook protocol delivers credentials from `container.credentials` and `services.<name>.credentials` to the backend. This demo does not create imagePullSecrets from them and rejects such requests; configure existing secrets through `GITEA_HOOK_IMAGE_PULL_SECRETS`.

Dockerfile builds, exec user overrides, privileged mode and arbitrary options/volumes are not implemented. Existing imagePullSecrets are supported. Readiness and container-step waits are limited to 180 seconds, deletion to 30 seconds. Cleaning up after a runner crash requires an external controller or TTL.

Protocol: [docs/container-hooks.md](../../../../docs/container-hooks.md). Setup: [README.md](README.md).
