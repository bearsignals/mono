BINARY_NAME=mono
INSTALL_PATH=$(HOME)/bin
DIST_DIR=dist

VERSION_PKG=github.com/gwuah/mono/internal/version
BRANCH=$(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown")
COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
VERSION?=$(shell git describe --tags --always 2>/dev/null || echo "dev")

LDFLAGS=-s -w -X $(VERSION_PKG).Branch=$(BRANCH) -X $(VERSION_PKG).Commit=$(COMMIT) -X $(VERSION_PKG).BuildTime=$(BUILD_TIME) -X $(VERSION_PKG).Version=$(VERSION)

PLATFORMS=darwin/amd64 darwin/arm64 linux/amd64 linux/arm64

.PHONY: build install clean release checksums

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) ./cmd/mono

install: build
	cp $(BINARY_NAME) $(INSTALL_PATH)/$(BINARY_NAME)
	rm -f $(BINARY_NAME)

clean:
	rm -f $(BINARY_NAME)
	rm -rf $(DIST_DIR)

release: clean
	mkdir -p $(DIST_DIR)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		output=$(DIST_DIR)/$(BINARY_NAME)-$$os-$$arch; \
		echo "Building $$os/$$arch..."; \
		GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$output ./cmd/mono || exit 1; \
	done
	$(MAKE) checksums

checksums:
	cd $(DIST_DIR) && shasum -a 256 * > checksums.txt
