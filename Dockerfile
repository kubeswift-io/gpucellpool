# Build the manager binary.
FROM docker.io/golang:1.26 AS builder
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
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -trimpath -ldflags="-s -w" -o manager ./cmd/manager

# Distroless static, non-root. This operator reconciles two API servers and
# nothing else: no shell, no package manager, no writable filesystem, and — unlike
# KubeSwift's launcher pods, which are privileged by deliberate design — no reason
# whatsoever for elevated privileges.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
