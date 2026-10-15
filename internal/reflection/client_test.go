package reflection_test

import (
	"context"
	"net"
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	grpcreflection "google.golang.org/grpc/reflection"
	rpbalpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"

	shopv1 "github.com/mauricioacevedo/grpc-explorer-mcp/internal/demo/shopv1"
	"github.com/mauricioacevedo/grpc-explorer-mcp/internal/reflection"
)

type orders struct {
	shopv1.UnimplementedOrderServiceServer
}

// dial starts a server with the order service and the given reflection setup.
func dial(t *testing.T, register func(*grpc.Server)) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	shopv1.RegisterOrderServiceServer(s, orders{})
	register(s)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestClient(t *testing.T) {
	setups := map[string]func(*grpc.Server){
		"v1 and v1alpha": func(s *grpc.Server) { grpcreflection.Register(s) },
		// Older servers only implement v1alpha; the client must fall back.
		"v1alpha only": func(s *grpc.Server) {
			rpbalpha.RegisterServerReflectionServer(s, grpcreflection.NewServer(grpcreflection.ServerOptions{Services: s}))
		},
	}
	for name, register := range setups {
		t.Run(name, func(t *testing.T) {
			c := reflection.NewClient(dial(t, register))
			ctx := context.Background()

			services, err := c.ListServices(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(services, []string{"shop.v1.OrderService"}) {
				t.Errorf("services = %v; reflection services should be hidden", services)
			}

			files, err := c.FilesForSymbol(ctx, "shop.v1.OrderService")
			if err != nil {
				t.Fatal(err)
			}
			// The dependency on google/protobuf/timestamp.proto must be resolved.
			if _, err := files.FindDescriptorByName("google.protobuf.Timestamp"); err != nil {
				t.Errorf("Timestamp dependency missing: %v", err)
			}
			if _, err := files.FindDescriptorByName("shop.v1.Order"); err != nil {
				t.Errorf("shop.v1.Order missing: %v", err)
			}

			if _, err := c.FilesForSymbol(ctx, "shop.v1.DoesNotExist"); err == nil {
				t.Error("expected error for unknown symbol")
			}
		})
	}
}
