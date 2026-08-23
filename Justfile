PROJECT := "go-galaxy"
BENCH := PROJECT + "-benchmark"
VERSION := `sh -c "git describe --tags --always --dirty --match 'v[0-9]*.[0-9]*.[0-9]*' 2>/dev/null || echo unknown"`
COMMIT := `git rev-parse --short HEAD`
DATE := `date -u +%Y-%m-%dT%H:%M:%SZ`
LDFLAGS := "-s -w" \
  + " -X main.Version=" + VERSION \
  + " -X main.Commit=" + COMMIT \
  + " -X main.Date=" + DATE \
  + " -X main.BuiltBy=just"

GOLANGCI_LINT_VERSION := "v2.13.1"
BENCH_LDFLAGS := "-s -w"

deps:
	@echo "===== Check deps for {{PROJECT}} ====="
	go mod tidy
	go mod vendor

lint:
	@echo "===== Lint {{PROJECT}} ====="
	@have=$(golangci-lint version --short 2>/dev/null || echo none); \
		want=$(echo "{{GOLANGCI_LINT_VERSION}}" | sed 's/^v//'); \
		if [ "$have" != "$want" ]; then \
			echo "golangci-lint $want is required, found $have" >&2; \
			echo "install it from https://golangci-lint.run/docs/welcome/install/" >&2; \
			exit 1; \
		fi
	golangci-lint run ./... --timeout=5m

test:
	@echo "===== Test {{PROJECT}} ====="
	go test ./...

check:
	@echo "===== Check {{PROJECT}} ====="
	go vet ./...
	go tool staticcheck ./...
	go tool govulncheck ./...
	go tool fieldalignment ./...
	go tool actionlint -shellcheck= -pyflakes=

fix:
	@echo "===== Fix {{PROJECT}} ====="
	go fix ./...
	go tool fieldalignment -fix ./...

run: check lint test
	@echo "===== Run {{PROJECT}} ====="
	go run -race ./cmd/{{ PROJECT }}/main.go

build: check lint test
	@echo "===== Build {{PROJECT}} ====="
	mkdir -p dist
	test -f dist/{{PROJECT}} && rm -f dist/{{PROJECT}} || echo "Not exist dist/{{PROJECT}}"
	CGO_ENABLED=0 go build -trimpath -ldflags="{{LDFLAGS}}"  -o ./dist/{{PROJECT}} ./cmd/{{ PROJECT }}/main.go
	@echo "===== Build {{BENCH}} ====="
	rm -f dist/{{BENCH}}
	CGO_ENABLED=0 go build -trimpath -ldflags="{{BENCH_LDFLAGS}}" -o ./dist/{{BENCH}} ./cmd/{{BENCH}}

build_linux: check
	@echo "===== Build {{PROJECT}} for Linux / amd64 ====="
	mkdir -p dist
	test -f dist/{{PROJECT}} && rm -f dist/{{PROJECT}} || echo "Not exist dist/{{PROJECT}}"
	GOOS="linux" GOARCH="amd64" CGO_ENABLED=0 go build -trimpath -ldflags="{{LDFLAGS}}" -o dist/{{PROJECT}} ./cmd/{{ PROJECT }}/main.go
	@echo "===== Build {{BENCH}} for Linux / amd64 ====="
	rm -f dist/{{BENCH}}
	GOOS="linux" GOARCH="amd64" CGO_ENABLED=0 go build -trimpath -ldflags="{{BENCH_LDFLAGS}}" -o dist/{{BENCH}} ./cmd/{{BENCH}}

oci executor="podman" tag="local": build_linux
	@echo "===== Build Local OCI {{PROJECT}} ====="
	rm -rf dist/oci
	mkdir -p dist/oci
	cp dist/{{PROJECT}} dist/oci/{{PROJECT}}
	{{executor}} build -t {{PROJECT}}:{{tag}} -f Dockerfile dist/oci
