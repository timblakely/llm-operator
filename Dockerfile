# Build the manager binary
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go sources
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build the independently deployed control- and data-plane binaries.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -a -o manager cmd/manager/main.go \
    && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -a -o llm-proxy ./cmd/llm-proxy

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot AS manager
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]

# The proxy is chart-packaged with the operator, but remains an independent
# request-serving process and image.
FROM gcr.io/distroless/static:nonroot AS proxy
WORKDIR /
COPY --from=builder /workspace/llm-proxy .
USER 65532:65532

ENTRYPOINT ["/llm-proxy"]

# The cache manager shares the proxy binary and source revision (it is
# invoked as "llm-proxy cache-manager"), but needs the official Hugging Face
# CLI for gated and Xet downloads, which the distroless proxy image cannot
# carry.
FROM python:3.12-slim AS cache-manager
RUN pip install --no-cache-dir huggingface_hub==0.34.4 hf_xet
COPY --from=builder /workspace/llm-proxy /llm-proxy
ENTRYPOINT ["/llm-proxy", "cache-manager"]
