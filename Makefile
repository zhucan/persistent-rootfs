GO ?= go
DOCKER ?= docker
IMAGE ?= persistent-rootfs-webhook:dev

.PHONY: all shim webhook test fmt webhook-image

all: shim webhook

shim:
	@mkdir -p build
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -o build/containerd-shim-persistent-v2 ./cmd/containerd-shim-persistent-v2

webhook:
	@mkdir -p build
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -o build/persistent-rootfs-webhook ./cmd/persistent-rootfs-webhook

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

webhook-image: webhook
	$(DOCKER) build -f deploy/Dockerfile.webhook -t $(IMAGE) .
