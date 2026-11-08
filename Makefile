BINARY := ccsession
GOFMT  ?= $(shell go env GOROOT)/bin/gofmt
PREFIX ?= /usr/local
BIN    := build/bin

.PHONY: all build test fmt vet check install uninstall clean help

all: build

help:
	@echo "make            same as make build"
	@echo "make build      build $(BIN)/$(BINARY) for this machine"
	@echo "make test       go test with the race detector"
	@echo "make fmt        gofmt -w ."
	@echo "make vet        go vet for linux and macOS"
	@echo "make check      gofmt, vet and tests"
	@echo "make install    copy $(BIN)/$(BINARY) to $(PREFIX)/bin; run make build first (PREFIX=~/.local for no sudo)"
	@echo "make uninstall  remove $(PREFIX)/bin/$(BINARY)"
	@echo "make clean      remove build/"

build:
	@mkdir -p $(BIN)
	go build -trimpath -o $(BIN)/$(BINARY) .

test:
	go test -race -count=1 ./...

fmt:
	$(GOFMT) -w .

vet:
	GOOS=linux go vet ./...
	GOOS=darwin go vet ./...

# The checks to run before every commit.
check: vet test
	@out=$$($(GOFMT) -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# Doesn't build: sudo resets PATH, so go usually isn't found under it.
# Run `make build` first, then `sudo make install`.
install:
	@if [ ! -f $(BIN)/$(BINARY) ]; then \
		echo "$(BIN)/$(BINARY) not found. Run 'make build' first (without sudo), then 'sudo make install'."; \
		exit 1; \
	fi
	@if [ -n "$$(find . -path ./build -prune -o -name '*.go' -newer $(BIN)/$(BINARY) -print | head -1)" ]; then \
		echo "warning: $(BIN)/$(BINARY) is older than the source; run 'make build' to update it"; \
	fi
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 755 $(BIN)/$(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)

clean:
	rm -rf build
