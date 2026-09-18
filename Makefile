PLUGIN := codex-header-rewrite
DIST := dist

.PHONY: test test-offline build-linux-amd64 clean

test:
	go test ./...

test-offline:
	go test -tags localtest -modfile=go.localtest.mod ./...

build-linux-amd64:
	mkdir -p $(DIST)
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -buildmode=c-shared -o $(DIST)/$(PLUGIN)-linux-amd64.so .
	rm -f $(DIST)/$(PLUGIN)-linux-amd64.h

clean:
	rm -rf $(DIST)
