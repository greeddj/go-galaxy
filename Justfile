PROJECT := "go-galaxy"
PKG := "github.com/greeddj/{{ PROJECT }}/cmd/{{ PROJECT }}"
VERSION := `sh -c 'git describe --tags --abbrev=0 2>/dev/null || git rev-parse --abbrev-ref HEAD'`
COMMIT := `git rev-parse --short HEAD`
FLAGS := "-s -w -extldflags '-static' -X {{PKG}}.Version={{VERSION}} -X {{PKG}}.Commit={{COMMIT}} -X {{PKG}}.Date={{DATE}} -X {{PKG}}.BuiltBy=just"


deps:
	@echo "===== Check deps for {{PROJECT}} ====="
	go mod tidy
	go mod vendor

lint:
	@echo "===== Lint {{PROJECT}} ====="
	golangci-lint run ./... --timeout=5m

test:
	@echo "===== Test {{PROJECT}} ====="
	go test ./...


check: deps
	@echo "===== Check {{PROJECT}} ====="
	go vet ./...
	go tool staticcheck ./...
	go tool govulncheck ./...

run: check lint test
	@echo "===== Run {{PROJECT}} ====="
	go run -race ./cmd/{{ PROJECT }}

build: check lint
	@echo "===== Build {{PROJECT}} ====="
	mkdir -p dist
	test -f dist/{{PROJECT}} && rm -f dist/{{PROJECT}} || echo "Not exist dist/{{PROJECT}}"
	CGO_ENABLED=0 go build -ldflags='{{FLAGS}}' -o dist/{{PROJECT}} ./cmd/{{ PROJECT }}

build_linux: check
	@echo "===== Build {{PROJECT}} for Linux / amd64 ====="
	mkdir -p dist
	test -f dist/{{PROJECT}} && rm -f dist/{{PROJECT}} || echo "Not exist dist/{{PROJECT}}"
	GOOS="linux" GOARCH="amd64" CGO_ENABLED=0 go build -ldflags='{{FLAGS}}' -o dist/{{PROJECT}} ./cmd/{{ PROJECT }}
