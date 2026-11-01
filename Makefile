BINARY    := grokbot-tunnel
PKG       := github.com/exa-pub/grokbot-tunnel
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X $(PKG)/internal/api/cli.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64
GOLANGCI_LINT ?= golangci-lint
FUZZTIME  ?= 10s

.PHONY: all build test test-race e2e systemd-test fuzz shellcheck lint proto proto-check dist clean

all: lint test build

# --- Build ---
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/grokbot-tunnel

# Release archives dist/grokbot-tunnel-<os>-<arch>.tar.gz and dist/checksums.txt.
dist:
	rm -rf dist && mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; name=$(BINARY)-$$os-$$arch; \
		echo "build $$name $(VERSION)"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$$name ./cmd/grokbot-tunnel || exit 1; \
		tar -C dist -czf dist/$$name.tar.gz $$name || exit 1; \
		rm dist/$$name; \
	done
	cd dist && sha256sum *.tar.gz > checksums.txt

# --- Checks ---
test:
	go test ./...

# COVERPROFILE=coverage.out also writes a coverage profile.
test-race:
	go test -race $(if $(COVERPROFILE),-coverprofile=$(COVERPROFILE)) ./...

# Runs the built binary against the fake backend in tests/e2e/fakesand.
e2e:
	go test -tags e2e -count=1 ./tests/e2e/...

# Installs, updates and uninstalls the service under systemd in a container;
# needs docker.
systemd-test:
	./tests/systemd/run.sh

# Runs every fuzz target for FUZZTIME.
fuzz:
	FUZZTIME=$(FUZZTIME) ./scripts/fuzz.sh

shellcheck:
	shellcheck install.sh scripts/*.sh tests/*/*.sh

lint:
	$(GOLANGCI_LINT) run ./...

# --- Proto ---
proto:
	./scripts/gen-proto.sh

# Fails if the committed generated code is out of date.
proto-check: proto
	git diff --exit-code -- internal/gen
	@test -z "$$(git status --porcelain -- internal/gen)" || { git status --porcelain -- internal/gen; exit 1; }

clean:
	rm -rf bin dist
