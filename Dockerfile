### BUILDER STAGE
#
#
FROM golang:1.27-alpine AS builder

# Do not remove `git` here, it is required for getting runner version when executing `make build`
RUN apk add --no-cache make git

ARG GOPROXY
ENV GOPROXY=${GOPROXY:-}

COPY . /opt/src/runner
WORKDIR /opt/src/runner

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    make clean && make build

### DIND VARIANT
#
#
FROM docker:29.9.3-dind AS dind

ARG VERSION=dev

LABEL org.opencontainers.image.source="https://gitea.com/gitea/runner"
LABEL org.opencontainers.image.version="${VERSION}"

# upgrade first: the docker base image can lag behind Alpine security fixes
RUN apk upgrade --no-cache && apk add --no-cache s6 bash git tzdata nftables

COPY --from=builder /opt/src/runner/gitea-runner /usr/local/bin/gitea-runner
COPY scripts/run.sh /usr/local/bin/run.sh
COPY scripts/s6 /etc/s6

VOLUME /data

ENV TINI_SUBREAPER=1

ENTRYPOINT ["s6-svscan","/etc/s6"]

### DIND-ROOTLESS VARIANT
#
#
FROM docker:29.9.3-dind-rootless AS dind-rootless

ARG VERSION=dev

LABEL org.opencontainers.image.source="https://gitea.com/gitea/runner"
LABEL org.opencontainers.image.version="${VERSION}"

USER root
# upgrade first: the docker base image can lag behind Alpine security fixes
RUN apk upgrade --no-cache && apk add --no-cache s6 bash git tzdata nftables

COPY --from=builder /opt/src/runner/gitea-runner /usr/local/bin/gitea-runner
COPY scripts/run.sh /usr/local/bin/run.sh
COPY scripts/s6 /etc/s6

VOLUME /data

RUN mkdir -p /data && chown -R rootless:rootless /etc/s6 /data

ENV DOCKER_HOST=unix:///run/user/1000/docker.sock
ENV TINI_SUBREAPER=1

USER rootless
ENTRYPOINT ["s6-svscan","/etc/s6"]

### CONTAINER HOOKS DEMO
FROM alpine:3.24 AS kubectl

ARG TARGETARCH
ARG KUBECTL_VERSION=v1.37.2

RUN apk add --no-cache curl ca-certificates && \
    curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" -o /kubectl && \
    curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256" -o /kubectl.sha256 && \
    printf '%s  /kubectl\n' "$(cat /kubectl.sha256)" | sha256sum -c - && chmod 755 /kubectl

FROM node:24-alpine3.24 AS container-hooks-k8s-demo

ARG VERSION=dev

LABEL org.opencontainers.image.source="https://gitea.com/gitea/runner"
LABEL org.opencontainers.image.version="${VERSION}"

RUN apk upgrade --no-cache && apk add --no-cache tini bash git tzdata ca-certificates

COPY --from=builder /opt/src/runner/gitea-runner /usr/local/bin/gitea-runner
COPY --from=kubectl /kubectl /usr/local/bin/kubectl
COPY scripts/run.sh /usr/local/bin/run.sh
COPY examples/container-hooks/demo/k8s/*.mjs /opt/container-hooks/k8s/
COPY examples/container-hooks/demo/k8s/runner.yaml /opt/container-hooks/k8s/runner.yaml

ENV CONFIG_FILE=/opt/container-hooks/k8s/runner.yaml
VOLUME /data
ENTRYPOINT ["/sbin/tini","--","run.sh"]

### BASIC VARIANT
#
#
FROM alpine:3.24 AS basic

ARG VERSION=dev

LABEL org.opencontainers.image.source="https://gitea.com/gitea/runner"
LABEL org.opencontainers.image.version="${VERSION}"

RUN apk upgrade --no-cache && apk add --no-cache tini bash git tzdata

COPY --from=builder /opt/src/runner/gitea-runner /usr/local/bin/gitea-runner
COPY scripts/run.sh /usr/local/bin/run.sh

VOLUME /data

ENTRYPOINT ["/sbin/tini","--","run.sh"]
