VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PREFIX  ?= $(HOME)/.local
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

.PHONY: build test install dist clean

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/codep .

test:
	go vet ./...
	go test -race -count=1 ./...

install: build
	install -d $(PREFIX)/bin
	install -m 755 bin/codep $(PREFIX)/bin/codep

# Release archives: dist/codep_<os>_<arch>.tar.gz and dist/checksums.txt
dist:
	rm -rf dist
	mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=codep_$${os}_$${arch}; \
		echo "build $$name"; \
		mkdir -p dist/$$name && \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$$name/codep . && \
		cp LICENSE README.md dist/$$name/ && \
		tar -C dist -czf dist/$$name.tar.gz $$name || exit 1; \
	done
	cd dist && shasum -a 256 *.tar.gz > checksums.txt

clean:
	rm -rf bin dist
