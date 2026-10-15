VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint proto demo run docker

build: ## Build both binaries into ./bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

test: ## Run tests with the race detector
	go test -race -cover ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

proto: ## Regenerate the demo service code (needs protoc, protoc-gen-go, protoc-gen-go-grpc)
	protoc -I proto \
		--go_out=. --go_opt=module=github.com/mauricioacevedo/grpc-explorer-mcp \
		--go-grpc_out=. --go-grpc_opt=module=github.com/mauricioacevedo/grpc-explorer-mcp \
		proto/shop/v1/shop.proto

demo: build ## Run the demo gRPC server on localhost:50051
	./bin/demo-server

docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t grpc-explorer-mcp:$(VERSION) .
