# Container hooks (Kubernetes API demo): run with kind

Run these Bash commands from the repository root. Docker, kind, kubectl and Git must be installed.

## 1. Set the Gitea connection

Create an empty repository on [Gitea.com](https://gitea.com), enable Actions and copy the runner registration token from that repository's settings. Replace `your-user` in the repository URL with your username:

```sh
export GITEA_INSTANCE_URL='https://gitea.com'
export GITEA_RUNNER_REGISTRATION_TOKEN='<repository-registration-token>'
export GITEA_TEST_REPOSITORY_URL='https://gitea.com/your-user/container-hooks-demo.git'

mkdir -p .tmp/container-hooks-demo
umask 077
cat > .tmp/container-hooks-demo/runner.env <<EOF
GITEA_INSTANCE_URL=$GITEA_INSTANCE_URL
GITEA_RUNNER_REGISTRATION_TOKEN=$GITEA_RUNNER_REGISTRATION_TOKEN
GITEA_RUNNER_NAME=container-hooks-demo
GITEA_RUNNER_LABELS=container-hooks-demo:docker://node:24-bookworm
GITEA_HOOK_NAMESPACE=container-hooks-demo
EOF
```

## 2. Build the image and create kind

```sh
docker build --target container-hooks-k8s-demo -t gitea-runner:container-hooks-k8s-demo .
kind create cluster --name container-hooks-demo \
  --config examples/container-hooks/demo/k8s/kind.yaml \
  --kubeconfig .tmp/container-hooks-demo/kubeconfig --wait 180s
export KUBECONFIG="$PWD/.tmp/container-hooks-demo/kubeconfig"
kubectl create namespace container-hooks-demo
```

## 3. Start the runner

Use **one** option: A or B. Both run jobs in kind.

### A. Runner in Docker

```sh
kind get kubeconfig --name container-hooks-demo --internal > .tmp/container-hooks-demo/docker-kubeconfig
docker run -d --rm --name container-hooks-demo --network kind \
  --env-file .tmp/container-hooks-demo/runner.env \
  -e KUBECONFIG=/config/kubeconfig \
  --mount type=bind,src="$PWD/.tmp/container-hooks-demo/docker-kubeconfig",dst=/config/kubeconfig,readonly \
  --mount type=volume,src=container-hooks-demo-data,dst=/data \
  gitea-runner:container-hooks-k8s-demo
docker logs --tail 50 container-hooks-demo
```

### B. Runner inside kind

```sh
kind load docker-image gitea-runner:container-hooks-k8s-demo --name container-hooks-demo
kubectl -n container-hooks-demo create secret generic runner-env \
  --from-env-file=.tmp/container-hooks-demo/runner.env
kubectl apply -f examples/container-hooks/demo/k8s/in-cluster.yaml
kubectl -n container-hooks-demo rollout status deployment/container-hooks-demo --timeout=180s
kubectl -n container-hooks-demo logs deployment/container-hooks-demo --tail=50
```

In the Gitea repository settings, `container-hooks-demo` should appear online with the `container-hooks-demo` label.

## 4. Run the workflow

Copy the example into the new repository and push it to Gitea:

```sh
mkdir -p .tmp/container-hooks-demo/repository
cp -R examples/container-hooks/demo/k8s/repository/. .tmp/container-hooks-demo/repository/
git -C .tmp/container-hooks-demo/repository init -b main
git -C .tmp/container-hooks-demo/repository remote add origin "$GITEA_TEST_REPOSITORY_URL"
git -C .tmp/container-hooks-demo/repository add .
git -C .tmp/container-hooks-demo/repository commit -m "test: exercise container hooks"
git -C .tmp/container-hooks-demo/repository push -u origin main
```

Open **Actions → Container hooks (Kubernetes API demo)**. Expect three successful jobs: two matrix `demo` jobs and `outputs`. The `demo` logs should contain `LOCAL_ACTION_OK`, `OUTPUTS_ENV_SERVICE_OK`, `CONTAINER_STEP_OK` and `POST_ACTION_STATE_OK`.

Watch job Pods:

```sh
kubectl -n container-hooks-demo get pods -w
```

Each job Pod is deleted after its job finishes. Use **Run workflow** in Gitea to run it again.

## 5. Remove the demo

For option A, first stop the Docker container and remove its data:

```sh
docker stop container-hooks-demo
docker volume rm container-hooks-demo-data
```

For either option, delete kind:

```sh
kind delete cluster --name container-hooks-demo
```

Hook settings and a separate protocol check: [configuration.md](configuration.md).
