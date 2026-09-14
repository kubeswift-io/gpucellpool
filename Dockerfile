# Build the manager binary.
FROM docker.io/golang:1.27 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Dependencies first, so source changes do not invalidate the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# CGO off and a static binary, so the image below needs no libc at all.
# VERSION is stamped into the binary so the CRD-drift report can print a fix
# command pointing at THIS release's manifest rather than at main. Unset in a local
# build, which is why the default in crdcheck is "main".
ARG VERSION=main
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -trimpath \
      -ldflags="-s -w -X github.com/kubeswift-io/gpucellpool/internal/crdcheck.Version=${VERSION}" \
      -o manager ./cmd/manager

# Distroless static, non-root. This operator reconciles two API servers and
# nothing else: no shell, no package manager, no writable filesystem, and — unlike
# KubeSwift's launcher pods, which are privileged by deliberate design — no reason
# whatsoever for elevated privileges.
FROM gcr.io/distroless/static:nonroot

# GitHub links a container package to a repository through this label, and that link
# is what lets the repository's Actions token push to the package. Without it, a first
# push from a workstation creates an ORPHAN package: the release workflow then fails
# with "403 Forbidden" on a blob it is not allowed to write, and the only other fix is
# a manual grant in Package Settings.
LABEL org.opencontainers.image.source="https://github.com/kubeswift-io/gpucellpool"
LABEL org.opencontainers.image.description="GPUCellPool operator: pools of VM-isolated, fractionally-shared GPU worker nodes"
LABEL org.opencontainers.image.licenses="Apache-2.0"
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
