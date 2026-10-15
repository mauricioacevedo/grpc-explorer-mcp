// Command grpc-explorer-mcp is an MCP server that lets AI assistants explore
// and call gRPC services through server reflection.
//
// It speaks MCP over stdio (for Claude Code and Claude Desktop) or over
// Streamable HTTP (for remote clients such as ChatGPT and claude.ai).
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauricioacevedo/grpc-explorer-mcp/internal/explorer"
	"github.com/mauricioacevedo/grpc-explorer-mcp/internal/mcpserver"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

type headerFlags map[string]string

func (h headerFlags) String() string { return fmt.Sprint(map[string]string(h)) }

func (h headerFlags) Set(v string) error {
	key, value, ok := strings.Cut(v, ":")
	if !ok || strings.TrimSpace(key) == "" {
		return fmt.Errorf("header %q must look like 'name: value'", v)
	}
	// Expand $VARS so secrets can stay in the environment instead of config files.
	h[strings.ToLower(strings.TrimSpace(key))] = os.ExpandEnv(strings.TrimSpace(value))
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "grpc-explorer-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	headers := headerFlags{}
	var (
		transport     = flag.String("transport", "stdio", "MCP transport: stdio or http")
		listen        = flag.String("listen", "127.0.0.1:8080", "address for the HTTP transport")
		httpToken     = flag.String("http-token", os.Getenv("GRPC_EXPLORER_HTTP_TOKEN"), "bearer token required by the HTTP transport (env GRPC_EXPLORER_HTTP_TOKEN)")
		target        = flag.String("target", os.Getenv("GRPC_EXPLORER_TARGET"), "default gRPC target host:port (env GRPC_EXPLORER_TARGET)")
		allowTargets  = flag.String("allow-targets", strings.Join(explorer.DefaultAllowedTargets, ","), "comma-separated host:port patterns the server may connect to; '*' allows any")
		allowCalls    = flag.Bool("allow-calls", false, "allow call_method to invoke unary RPCs (read-only by default)")
		useTLS        = flag.Bool("tls", false, "connect to targets using TLS")
		tlsSkipVerify = flag.Bool("tls-skip-verify", false, "skip TLS certificate verification (development only)")
		timeout       = flag.Duration("timeout", 10*time.Second, "timeout for each reflection request or call")
		maxRespBytes  = flag.Int("max-response-bytes", 4<<20, "maximum size of a call response")
		showVersion   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Var(headers, "header", "gRPC metadata sent with every request, as 'name: value' ($VARS are expanded); repeatable")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	// stdout carries the MCP protocol in stdio mode, so logs go to stderr.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ex, err := explorer.New(explorer.Options{
		AllowedTargets:   strings.Split(*allowTargets, ","),
		DefaultTarget:    *target,
		AllowCalls:       *allowCalls,
		Timeout:          *timeout,
		Headers:          headers,
		TLS:              *useTLS,
		TLSSkipVerify:    *tlsSkipVerify,
		MaxResponseBytes: *maxRespBytes,
		UserAgent:        "grpc-explorer-mcp/" + version,
	})
	if err != nil {
		return err
	}
	defer func() { _ = ex.Close() }()

	server := mcpserver.New(ex, version)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("starting", "version", version, "transport", *transport, "default_target", *target, "allow_calls", *allowCalls)
	switch *transport {
	case "stdio":
		return server.Run(ctx, &mcp.StdioTransport{})
	case "http":
		return serveHTTP(ctx, logger, server, *listen, *httpToken)
	default:
		return fmt.Errorf("unknown transport %q (want stdio or http)", *transport)
	}
}

func serveHTTP(ctx context.Context, logger *slog.Logger, server *mcp.Server, addr, token string) error {
	mux := http.NewServeMux()
	mux.Handle("/mcp", requireToken(token, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if token == "" {
		logger.Warn("HTTP transport has no --http-token; anyone who can reach it can use it")
	}
	logger.Info("listening", "url", "http://"+addr+"/mcp")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
