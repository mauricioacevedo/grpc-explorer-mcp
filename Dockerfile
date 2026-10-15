# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/grpc-explorer-mcp ./cmd/grpc-explorer-mcp \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/demo-server ./cmd/demo-server

# Demo backend used by docker-compose.
FROM gcr.io/distroless/static-debian12:nonroot AS demo
COPY --from=build /out/demo-server /demo-server
EXPOSE 50051
ENTRYPOINT ["/demo-server", "-listen", "0.0.0.0:50051"]

# Default image: the MCP server.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/grpc-explorer-mcp /grpc-explorer-mcp
ENTRYPOINT ["/grpc-explorer-mcp"]
