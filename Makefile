# Makefile for quantifai-sync — the Quantifai telemetry sync agent.

BINARY      := quantifai-sync
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS     := -ldflags "-X github.com/quantifai/sync/cmd.Version=$(VERSION)"
INSTALL_DIR ?= $(HOME)/.local/bin

.PHONY: build install uninstall test test-race clean cross-build formula pkg

## build: Compile the binary for the current platform
build:
	go build $(LDFLAGS) -o $(BINARY) .

## install: Build, copy to INSTALL_DIR, and register the OS service
install: build
	@mkdir -p $(INSTALL_DIR)
	cp $(BINARY) $(INSTALL_DIR)/$(BINARY)
	$(INSTALL_DIR)/$(BINARY) install

## uninstall: Stop/remove the OS service and delete the binary
uninstall:
	$(INSTALL_DIR)/$(BINARY) uninstall || true
	rm -f $(INSTALL_DIR)/$(BINARY)

## test: Run all tests
test:
	go test ./...

## test-race: Run all tests with race detector
test-race:
	go test -race ./...

## clean: Remove build artifacts
clean:
	rm -f $(BINARY)
	rm -rf bin/

## cross-build: Build for all supported platforms
## Darwin builds need CGO_ENABLED=1 for systray; Linux/Windows use CGO_ENABLED=0.
cross-build:
	@mkdir -p bin
	CGO_ENABLED=1 GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY)-darwin-amd64 .
	CGO_ENABLED=1 GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o bin/$(BINARY)-darwin-arm64 .
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY)-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build $(LDFLAGS) -o bin/$(BINARY)-linux-arm64 .
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY)-windows-amd64.exe .

## formula: Render the Homebrew formula for VERSION into bin/quantifai-sync.rb
formula:
	packaging/homebrew/update-formula.sh $(VERSION) bin/quantifai-sync.rb

## pkg: Build macOS .pkg installer (macOS only)
pkg:
	packaging/macos/build-pkg.sh $(VERSION) $(shell uname -m)
