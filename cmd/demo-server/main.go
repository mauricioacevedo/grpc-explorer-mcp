// Command demo-server runs a small gRPC shop backend with reflection and health
// enabled, so grpc-explorer-mcp can be tried without any other setup.
package main

import (
	"flag"
	"log"
	"net"

	"google.golang.org/grpc"

	"github.com/mauricioacevedo/grpc-explorer-mcp/internal/demo"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:50051", "address to listen on")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	s := grpc.NewServer()
	demo.Register(s)
	log.Printf("demo gRPC server listening on %s (services: shop.v1.OrderService, shop.v1.InventoryService)", lis.Addr())
	if err := s.Serve(lis); err != nil {
		log.Fatal(err)
	}
}
