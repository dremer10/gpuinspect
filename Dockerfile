# syntax=docker/dockerfile:1
#
# Build gpuinspect as static binaries for the node (linux/amd64)
# and for operator laptops (darwin arm64/amd64, linux/arm64).
#
# Usage (normally via `make docker-build`):
#   docker build --build-arg VERSION=$(date +%Y.%m.%d) --output type=local,dest=../../bin .
#   # pin the Go toolchain: --build-arg GO_VERSION=1.23
#
ARG GO_VERSION=1.23
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN GOOS=linux  GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gpuinspect-linux-amd64 . && \
    GOOS=linux  GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gpuinspect-linux-arm64 . && \
    GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gpuinspect-darwin-arm64 . && \
    GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gpuinspect-darwin-amd64 .

# Final stage contains ONLY the binaries so `--output type=local` exports just them.
FROM scratch AS artifacts
COPY --from=build /out/ /
