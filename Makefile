PLUGIN := codex-header-rewrite
DIST := dist
RELEASE_DIR := $(CURDIR)/.release
VERSION ?= 0.0.0-dev
LDFLAGS := -s -w -X main.pluginVersion=$(VERSION)

.PHONY: test test-offline build-linux-amd64 build-linux-arm64 package package-test release-local clean

test:
	go test ./...

test-offline:
	go test -tags localtest -modfile=go.localtest.mod ./...

build-linux-amd64:
	mkdir -p $(DIST)
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -buildmode=c-shared -o $(DIST)/$(PLUGIN)-linux-amd64.so .
	rm -f $(DIST)/$(PLUGIN)-linux-amd64.h

# Release build for one architecture, stamped with VERSION. arm64 needs
# aarch64-linux-gnu-gcc because the plugin is built as a cgo c-shared library.
build-linux-arm64:
	mkdir -p $(DIST)
	CC=aarch64-linux-gnu-gcc CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -trimpath -buildmode=c-shared -o $(DIST)/$(PLUGIN)-linux-arm64.so .
	rm -f $(DIST)/$(PLUGIN)-linux-arm64.h

# Same layout the Release workflow publishes: plugin-store zips, per-library
# checksums, and one aggregated checksums.txt.
package:
	mkdir -p $(RELEASE_DIR)
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -buildmode=c-shared -o $(RELEASE_DIR)/$(PLUGIN)-linux-amd64.so .
	CC=aarch64-linux-gnu-gcc CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -buildmode=c-shared -o $(RELEASE_DIR)/$(PLUGIN)-linux-arm64.so .
	rm -f $(RELEASE_DIR)/*.h
	scripts/package-release.sh $(VERSION) $(RELEASE_DIR)

package-test:
	scripts/package_release_test.sh $(RELEASE_DIR) $(VERSION)

release-local: package package-test

clean:
	rm -rf $(DIST) $(RELEASE_DIR)
