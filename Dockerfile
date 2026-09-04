# Build the manager binary.
#
# --platform=$BUILDPLATFORM pins the builder stage to the HOST's architecture
# and cross-compiles to $TARGETARCH below, instead of running the whole Go
# toolchain under QEMU: emulated builds are an order of magnitude slower, and
# nothing here needs CGO. Dockerfile.shim, Dockerfile.fakeengine and
# Dockerfile.loadgen all do this already; this one was the outlier, which is
# invisible on an amd64 runner and painful on an arm64 laptop building for
# linux/amd64. `make docker-buildx` existed largely to sed this line in.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
