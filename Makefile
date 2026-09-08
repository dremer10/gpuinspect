# gpuinspect — build orchestration
#
# `make build` (default) produces:
#   ../../bin/gpuinspect              native binary (on $PATH via team/amerten/bin)
#   ../../bin/gpuinspect-linux-amd64  pushed to x86 nodes by remote mode
#   ../../bin/gpuinspect-linux-arm64  pushed to aarch64 nodes (GH200/GB200)
# Remote mode probes the node arch and picks the matching binary; it looks for
# the linux binaries next to the running binary, so all land in ../../bin/.

VERSION    ?= $(shell date +%Y.%m.%d)
GO_VERSION ?= 1.23
LDFLAGS     = -s -w -X main.version=$(VERSION)
BINDIR      = ../../bin
NAME        = gpuinspect

.PHONY: build release docker-build test lint fmt clean

build:
	mkdir -p $(BINDIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(NAME) .
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(NAME)-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(NAME)-linux-arm64 .

release: build
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(NAME)-darwin-amd64 .
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(NAME)-darwin-arm64 .

# Optional: build all platform binaries inside Docker (pins the Go toolchain
# without needing one installed locally). Exports only the binaries to ../../bin.
docker-build:
	docker build --build-arg VERSION=$(VERSION) --build-arg GO_VERSION=$(GO_VERSION) --output type=local,dest=$(BINDIR) .

test:
	go test ./...

# Best-effort: mirrors the golangci-lint hook run by the repo pre-commit CI.
lint:
	golangci-lint run

fmt:
	gofmt -l -w .

clean:
	rm -f $(BINDIR)/$(NAME) \
	      $(BINDIR)/$(NAME)-linux-amd64 \
	      $(BINDIR)/$(NAME)-linux-arm64 \
	      $(BINDIR)/$(NAME)-darwin-amd64 \
	      $(BINDIR)/$(NAME)-darwin-arm64
