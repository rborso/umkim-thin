# umkim-thin console/API image: the binary plus the helm and kubectl it drives.
FROM golang:1.24-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/umkim-thin ./cmd/umkim-thin

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
LABEL org.opencontainers.image.title="umkim-thin" \
      org.opencontainers.image.description="Single-cluster self-hosted app platform console" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/rborso/umkim-thin"

ARG HELM_VERSION=3.16.3
ARG KUBECTL_VERSION=1.32.0
RUN set -eux; \
    apk add --no-cache ca-certificates bash curl tzdata nfs-utils; \
    adduser -D -H -u 65532 -s /sbin/nologin app; \
    arch=$(uname -m); \
    case "$arch" in x86_64) a=amd64 ;; aarch64) a=arm64 ;; *) echo "unsupported arch $arch" >&2; exit 1 ;; esac; \
    helm_tar="helm-v${HELM_VERSION}-linux-${a}.tar.gz"; \
    curl -fsSL "https://get.helm.sh/${helm_tar}" -o "/tmp/${helm_tar}"; \
    curl -fsSL "https://get.helm.sh/${helm_tar}.sha256sum" -o "/tmp/${helm_tar}.sha256"; \
    (cd /tmp && sha256sum -c "${helm_tar}.sha256"); \
    tar -xzf "/tmp/${helm_tar}" -C /tmp; \
    mv "/tmp/linux-${a}/helm" /usr/local/bin/helm; \
    curl -fsSL "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${a}/kubectl" -o /tmp/kubectl; \
    curl -fsSL "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${a}/kubectl.sha256" -o /tmp/kubectl.sha256; \
    printf '%s  %s\n' "$(cat /tmp/kubectl.sha256)" /tmp/kubectl | sha256sum -c -; \
    mv /tmp/kubectl /usr/local/bin/kubectl; \
    chmod +x /usr/local/bin/helm /usr/local/bin/kubectl; \
    rm -rf "/tmp/${helm_tar}" "/tmp/${helm_tar}.sha256" /tmp/linux-* /tmp/kubectl.sha256; \
    mkdir -p /opt/umkim-thin
ARG UMKIM_THIN_GIT_SHA=dev
RUN printf '%s\n' "$UMKIM_THIN_GIT_SHA" > /opt/umkim-thin/git-sha
# The release tag the image was built from. The console compares it with the
# published releases; an untagged local build stays "dev" and the check is off.
ARG UMKIM_THIN_VERSION=dev
RUN printf '%s\n' "$UMKIM_THIN_VERSION" > /opt/umkim-thin/version

COPY --from=build /out/umkim-thin /usr/local/bin/umkim-thin
COPY LICENSE NOTICE /usr/share/licenses/umkim-thin/
COPY catalog /opt/umkim-thin/catalog
# The console is the only source of the bootstrap build context: the builder Job
# copies it from this image, so the API never accepts one from a caller.
COPY images/servarr-bootstrap /opt/umkim-thin/bootstrap

ENV HOME=/tmp
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/umkim-thin"]
CMD ["serve", "--addr", ":8080", "--catalog", "/opt/umkim-thin/catalog"]
