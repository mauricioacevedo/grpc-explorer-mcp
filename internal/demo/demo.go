// Package demo implements a small in-memory shop backend used by the demo
// server and the integration tests. It registers reflection and health so
// grpc-explorer-mcp can discover it without any .proto files.
package demo

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	shopv1 "github.com/mauricioacevedo/grpc-explorer-mcp/internal/demo/shopv1"
)

// Register adds the demo services, gRPC health and server reflection to s.
func Register(s *grpc.Server) {
	store := newStore()
	shopv1.RegisterOrderServiceServer(s, &orderService{store: store})
	shopv1.RegisterInventoryServiceServer(s, &inventoryService{store: store})

	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	hs.SetServingStatus("shop.v1.OrderService", healthpb.HealthCheckResponse_SERVING)
	hs.SetServingStatus("shop.v1.InventoryService", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(s, hs)

	reflection.Register(s)
}

type store struct {
	mu     sync.Mutex
	orders map[string]*shopv1.Order
	stock  map[string]*shopv1.Stock
	nextID int
}

func newStore() *store {
	created := timestamppb.New(time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC))
	usd := func(units int64) *shopv1.Money { return &shopv1.Money{CurrencyCode: "USD", Units: units} }
	s := &store{
		orders: map[string]*shopv1.Order{
			"ord-1001": {
				Id: "ord-1001", CustomerId: "cus-42", Status: shopv1.OrderStatus_ORDER_STATUS_SHIPPED,
				Items:     []*shopv1.LineItem{{Sku: "KB-01", Quantity: 1, UnitPrice: usd(89)}},
				Total:     usd(89),
				CreatedAt: created,
				Labels:    map[string]string{"channel": "web"},
			},
			"ord-1002": {
				Id: "ord-1002", CustomerId: "cus-42", Status: shopv1.OrderStatus_ORDER_STATUS_PENDING,
				Items:     []*shopv1.LineItem{{Sku: "MS-07", Quantity: 2, UnitPrice: usd(25)}},
				Total:     usd(50),
				CreatedAt: created,
			},
			"ord-1003": {
				Id: "ord-1003", CustomerId: "cus-7", Status: shopv1.OrderStatus_ORDER_STATUS_PAID,
				Items:     []*shopv1.LineItem{{Sku: "MN-27", Quantity: 1, UnitPrice: usd(310)}},
				Total:     usd(310),
				CreatedAt: created,
				Labels:    map[string]string{"channel": "mobile"},
			},
		},
		stock: map[string]*shopv1.Stock{
			"KB-01": {Sku: "KB-01", Available: 14, Location: &shopv1.Stock_Warehouse{Warehouse: "MDE-1"}},
			"MS-07": {Sku: "MS-07", Available: 0, Location: &shopv1.Stock_Warehouse{Warehouse: "MDE-1"}},
			"MN-27": {Sku: "MN-27", Available: 3, Location: &shopv1.Stock_StoreId{StoreId: "store-poblado"}},
		},
		nextID: 1004,
	}
	return s
}

type orderService struct {
	shopv1.UnimplementedOrderServiceServer
	store *store
}

func (o *orderService) GetOrder(_ context.Context, req *shopv1.GetOrderRequest) (*shopv1.Order, error) {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	order, ok := o.store.orders[req.GetId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "order %q not found", req.GetId())
	}
	return proto.Clone(order).(*shopv1.Order), nil
}

func (o *orderService) ListOrders(_ context.Context, req *shopv1.ListOrdersRequest) (*shopv1.ListOrdersResponse, error) {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	resp := &shopv1.ListOrdersResponse{}
	for _, order := range o.store.orders {
		if req.GetCustomerId() != "" && order.GetCustomerId() != req.GetCustomerId() {
			continue
		}
		if req.GetStatus() != shopv1.OrderStatus_ORDER_STATUS_UNSPECIFIED && order.GetStatus() != req.GetStatus() {
			continue
		}
		resp.Orders = append(resp.Orders, proto.Clone(order).(*shopv1.Order))
	}
	sort.Slice(resp.Orders, func(i, j int) bool { return resp.Orders[i].GetId() < resp.Orders[j].GetId() })
	if n := int(req.GetPageSize()); n > 0 && n < len(resp.Orders) {
		resp.Orders = resp.Orders[:n]
	}
	return resp, nil
}

func (o *orderService) CreateOrder(_ context.Context, req *shopv1.CreateOrderRequest) (*shopv1.Order, error) {
	if req.GetCustomerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "customer_id is required")
	}
	if len(req.GetItems()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one item is required")
	}
	total := &shopv1.Money{CurrencyCode: "USD"}
	for _, item := range req.GetItems() {
		if item.GetQuantity() <= 0 {
			return nil, status.Errorf(codes.InvalidArgument, "quantity for %q must be positive", item.GetSku())
		}
		total.Units += item.GetUnitPrice().GetUnits() * int64(item.GetQuantity())
	}

	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	order := &shopv1.Order{
		Id:         fmt.Sprintf("ord-%d", o.store.nextID),
		CustomerId: req.GetCustomerId(),
		Items:      req.GetItems(),
		Status:     shopv1.OrderStatus_ORDER_STATUS_PENDING,
		Total:      total,
		CreatedAt:  timestamppb.Now(),
	}
	o.store.nextID++
	o.store.orders[order.Id] = order
	return proto.Clone(order).(*shopv1.Order), nil
}

func (o *orderService) WatchOrders(req *shopv1.WatchOrdersRequest, stream grpc.ServerStreamingServer[shopv1.Order]) error {
	resp, err := o.ListOrders(stream.Context(), &shopv1.ListOrdersRequest{CustomerId: req.GetCustomerId()})
	if err != nil {
		return err
	}
	for _, order := range resp.GetOrders() {
		if err := stream.Send(order); err != nil {
			return err
		}
	}
	return nil
}

type inventoryService struct {
	shopv1.UnimplementedInventoryServiceServer
	store *store
}

func (i *inventoryService) GetStock(_ context.Context, req *shopv1.GetStockRequest) (*shopv1.Stock, error) {
	i.store.mu.Lock()
	defer i.store.mu.Unlock()
	stock, ok := i.store.stock[req.GetSku()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "sku %q not found", req.GetSku())
	}
	return proto.Clone(stock).(*shopv1.Stock), nil
}
