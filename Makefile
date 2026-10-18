GO ?= go
BINARY := nghtwv
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt clean cross-check

all: vet build

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) .

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

clean:
	$(GO) clean
	rm -f $(BINARY) $(BINARY).exe

# cross-check builds the two non-host targets of the release matrix.
cross-check:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o $(BINARY).exe .
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -o /dev/null .
