.PHONY: build bench-build build-all test test-coverage clean lint fmt install run watch help

# Build variables
BINARY := sieve
BENCH_BINARY := bench
LDFLAGS := -ldflags="-s -w"

# Build targets
build:
	@echo "🔨 Building sieve..."
	@mkdir -p bin
	@go build $(LDFLAGS) -o bin/$(BINARY) ./cmd/sieve

bench-build:
	@echo "🔨 Building benchmark tool..."
	@mkdir -p bin
	@go build $(LDFLAGS) -o bin/$(BENCH_BINARY) ./cmd/bench

build-all: build bench-build
	@echo "✓ All binaries built successfully"

# Testing
test:
	@echo "🧪 Running tests..."
	@go test -v -race -timeout 30s ./...

test-coverage:
	@echo "🧪 Running tests with coverage..."
	@go test -v -race -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "✓ Coverage report: coverage.html"

# Development
clean:
	@echo "🧹 Cleaning build artifacts..."
	@rm -rf bin/ dist/ coverage.out coverage.html
	@go clean

lint:
	@echo "📝 Running linter..."
	@golangci-lint run ./...

fmt:
	@echo "📝 Formatting code..."
	@go fmt ./...

install: build
	@echo "📦 Installing sieve to $(GOPATH)/bin/..."
	@cp bin/$(BINARY) $(GOPATH)/bin/

run: build
	@echo "▶️  Running sieve (port 4141)..."
	@./bin/$(BINARY)

# Release builds
release:
	@echo "🚀 Building releases..."
	@mkdir -p dist
	@GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY)-linux-amd64   ./cmd/sieve
	@GOOS=linux   GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BINARY)-linux-arm64   ./cmd/sieve
	@GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY)-darwin-amd64  ./cmd/sieve
	@GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BINARY)-darwin-arm64  ./cmd/sieve
	@GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY)-windows-amd64.exe ./cmd/sieve
	@echo "✓ Release binaries in dist/"

help:
	@echo "Sieve — Token-saving LLM proxy"
	@echo ""
	@echo "Available targets:"
	@echo "  make build         - Build sieve binary"
	@echo "  make bench-build   - Build benchmark tool"
	@echo "  make build-all     - Build all binaries"
	@echo "  make test          - Run tests"
	@echo "  make test-coverage - Run tests with coverage report"
	@echo "  make run           - Build and run sieve"
	@echo "  make clean         - Clean build artifacts"
	@echo "  make lint          - Run linter"
	@echo "  make fmt           - Format code"
	@echo "  make install       - Build and install to GOPATH"
	@echo "  make release       - Build cross-platform binaries"
	@echo "  make help          - Show this message"
