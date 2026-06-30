BINARY := sieve

.PHONY: run build clean release

run:
	go run .

build:
	go build -ldflags="-s -w" -o $(BINARY) .

# Cross-compile single static binaries for common targets
release:
	GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o dist/$(BINARY)-linux-amd64   .
	GOOS=linux   GOARCH=arm64 go build -ldflags="-s -w" -o dist/$(BINARY)-linux-arm64   .
	GOOS=darwin  GOARCH=amd64 go build -ldflags="-s -w" -o dist/$(BINARY)-darwin-amd64  .
	GOOS=darwin  GOARCH=arm64 go build -ldflags="-s -w" -o dist/$(BINARY)-darwin-arm64  .
	GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dist/$(BINARY)-windows-amd64.exe .

clean:
	rm -f $(BINARY)
	rm -rf dist
