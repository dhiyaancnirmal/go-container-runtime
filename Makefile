GO ?= go
# Keep builds to two parallel jobs on shared hosts.
GOFLAGS += -p=2

.PHONY: build test rootfs bench clean

build:
	$(GO) build $(GOFLAGS) -o bin/gcr ./cmd/gcr
	$(GO) build $(GOFLAGS) -o bin/gcr-bench ./cmd/gcr-bench

rootfs:
	./scripts/fetch-rootfs.sh rootfs

# Integration tests need root. Build as the current user, run as root.
test: rootfs
	$(GO) vet ./...
	$(GO) test $(GOFLAGS) -c -o bin/gcr.test ./cmd/gcr
	sudo GCR_ROOTFS=$(CURDIR)/rootfs bin/gcr.test -test.v -test.count=1

bench: build rootfs
	sudo bin/gcr-bench -gcr bin/gcr -rootfs $(CURDIR)/rootfs -out results

clean:
	rm -rf bin
