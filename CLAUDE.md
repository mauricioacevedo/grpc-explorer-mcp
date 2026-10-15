# grpc-explorer-mcp

MCP server (Go) that explores and calls gRPC services through server reflection.

## Commands
- `make test`: `go test -race -cover ./...`. Run it before every commit.
- `make lint`: golangci-lint v2 (config in `.golangci.yml`).
- `make proto`: regenerate `internal/demo/shopv1` after editing `proto/shop/v1/shop.proto`.
- `go run ./cmd/demo-server` and `go run ./cmd/grpc-explorer-mcp --target localhost:50051 --allow-calls` for manual testing.

## Layout
- `internal/reflection`: reflection protocol only. No policy or MCP concepts here.
- `internal/explorer`: all safety rules (target allowlist, read-only mode, timeouts, size limits) live here, so every transport gets them.
- `internal/mcpserver`: thin tool definitions. Handlers return Go errors, and the SDK turns them into `IsError` results the model can read.

## Conventions
- Tool errors must tell the model how to recover (e.g. "use describe_method", "restart with --allow-calls").
- Non-OK gRPC statuses from a call are results (`code`/`message`), not errors.
- New tools need annotations (`ReadOnlyHint` etc.) and a test in `internal/mcpserver/server_test.go`.
- Integration tests use the demo service on a random `127.0.0.1` port; never depend on external servers.
- Never write to stdout outside the MCP transport: in stdio mode it carries the protocol. Log to stderr.
