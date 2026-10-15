package explorer_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/mauricioacevedo/grpc-explorer-mcp/internal/demo"
	"github.com/mauricioacevedo/grpc-explorer-mcp/internal/explorer"
)

// startDemo runs the demo shop server on a random local port.
func startDemo(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	demo.Register(s)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

func newExplorer(t *testing.T, opts explorer.Options) *explorer.Explorer {
	t.Helper()
	ex, err := explorer.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ex.Close() })
	return ex
}

func TestListServices(t *testing.T) {
	target := startDemo(t)
	ex := newExplorer(t, explorer.Options{DefaultTarget: target})

	services, err := ex.ListServices(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"grpc.health.v1.Health", "shop.v1.InventoryService", "shop.v1.OrderService"}
	if !slices.Equal(services, want) {
		t.Errorf("services = %v, want %v", services, want)
	}
}

func TestDescribeService(t *testing.T) {
	target := startDemo(t)
	ex := newExplorer(t, explorer.Options{})

	info, err := ex.DescribeService(context.Background(), target, "shop.v1.OrderService")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]explorer.MethodInfo{}
	for _, m := range info.Methods {
		byName[m.Name] = m
	}
	get := byName["GetOrder"]
	if get.FullName != "shop.v1.OrderService/GetOrder" || get.InputType != "shop.v1.GetOrderRequest" || !get.Callable {
		t.Errorf("GetOrder = %+v", get)
	}
	watch := byName["WatchOrders"]
	if !watch.ServerStreaming || watch.Callable {
		t.Errorf("WatchOrders = %+v, want server streaming and not callable", watch)
	}

	if _, err := ex.DescribeService(context.Background(), target, "shop.v1.Missing"); err == nil {
		t.Error("expected error for unknown service")
	}
}

func TestDescribeMethodBuildsUsableExample(t *testing.T) {
	target := startDemo(t)
	ex := newExplorer(t, explorer.Options{AllowCalls: true})
	ctx := context.Background()

	detail, err := ex.DescribeMethod(ctx, target, "shop.v1.OrderService.CreateOrder")
	if err != nil {
		t.Fatal(err)
	}
	example, ok := detail.Request.Example.(map[string]any)
	if !ok {
		t.Fatalf("example is %T", detail.Request.Example)
	}
	if _, ok := example["customerId"]; !ok {
		t.Errorf("example %v is missing customerId", example)
	}

	// The response example covers enums, maps, nested messages and well-known types.
	resp := detail.Response.Example.(map[string]any)
	if resp["status"] != "ORDER_STATUS_PENDING" {
		t.Errorf("status example = %v", resp["status"])
	}
	if resp["createdAt"] != "2026-01-01T00:00:00Z" {
		t.Errorf("createdAt example = %v", resp["createdAt"])
	}
	if _, ok := resp["labels"].(map[string]any); !ok {
		t.Errorf("labels example = %v", resp["labels"])
	}

	// Every generated example must be accepted by the server's own schema.
	body, _ := json.Marshal(detail.Response.Example)
	if _, err := ex.Call(ctx, target, "shop.v1.OrderService/GetOrder", body, nil); err == nil {
		t.Error("expected an error: an Order is not a GetOrderRequest")
	}
	body, _ = json.Marshal(detail.Request.Example)
	if _, err := ex.Call(ctx, target, "shop.v1.OrderService/CreateOrder", body, nil); err != nil {
		t.Errorf("example request rejected: %v", err)
	}
}

func TestDescribeMessageOneofExampleIsValid(t *testing.T) {
	target := startDemo(t)
	ex := newExplorer(t, explorer.Options{})

	info, err := ex.DescribeMessage(context.Background(), target, "shop.v1.Stock")
	if err != nil {
		t.Fatal(err)
	}
	example := info.Example.(map[string]any)
	_, hasWarehouse := example["warehouse"]
	_, hasStore := example["storeId"]
	if !hasWarehouse || hasStore {
		t.Errorf("oneof example should set only the first member, got %v", example)
	}
	for _, f := range info.Fields {
		if f.Name == "store_id" && f.OneOf != "location" {
			t.Errorf("store_id oneof = %q", f.OneOf)
		}
	}
}

func TestCall(t *testing.T) {
	target := startDemo(t)
	ex := newExplorer(t, explorer.Options{DefaultTarget: target, AllowCalls: true})
	ctx := context.Background()

	res, err := ex.Call(ctx, "", "shop.v1.OrderService/GetOrder", json.RawMessage(`{"id":"ord-1001"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != "OK" {
		t.Fatalf("code = %s: %s", res.Code, res.Message)
	}
	order := res.Response.(map[string]any)
	if order["customerId"] != "cus-42" || order["status"] != "ORDER_STATUS_SHIPPED" {
		t.Errorf("order = %v", order)
	}

	// Server errors are reported as a status, not a Go error.
	res, err = ex.Call(ctx, "", "/shop.v1.OrderService/GetOrder", json.RawMessage(`{"id":"nope"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != "NotFound" || !strings.Contains(res.Message, "nope") {
		t.Errorf("got %s %q, want NotFound", res.Code, res.Message)
	}
}

func TestCallRejections(t *testing.T) {
	target := startDemo(t)
	ctx := context.Background()

	readOnly := newExplorer(t, explorer.Options{DefaultTarget: target})
	if _, err := readOnly.Call(ctx, "", "shop.v1.OrderService/GetOrder", nil, nil); !errors.Is(err, explorer.ErrCallsDisabled) {
		t.Errorf("read-only call err = %v, want ErrCallsDisabled", err)
	}

	ex := newExplorer(t, explorer.Options{DefaultTarget: target, AllowCalls: true})
	tests := map[string]struct {
		method, body, wantErr string
	}{
		"streaming":     {"shop.v1.OrderService/WatchOrders", `{}`, "streaming"},
		"unknown field": {"shop.v1.OrderService/GetOrder", `{"orderId":"x"}`, "does not match"},
		"bad method":    {"GetOrder", `{}`, "package.Service/Method"},
		"no method":     {"shop.v1.OrderService/Nope", `{}`, "not found"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ex.Call(ctx, "", tc.method, json.RawMessage(tc.body), nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestTargetPolicyIsEnforced(t *testing.T) {
	ex := newExplorer(t, explorer.Options{AllowedTargets: []string{"localhost:*"}})
	_, err := ex.ListServices(context.Background(), "10.0.0.1:50051")
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("err = %v, want not allowed", err)
	}
	if _, err := explorer.New(explorer.Options{DefaultTarget: "example.com:443"}); err == nil {
		t.Error("expected default target outside the allowlist to be rejected")
	}
}

func TestHealth(t *testing.T) {
	target := startDemo(t)
	ex := newExplorer(t, explorer.Options{DefaultTarget: target})
	ctx := context.Background()

	for service, want := range map[string]string{
		"":                     "SERVING",
		"shop.v1.OrderService": "SERVING",
		"unknown.Service":      "SERVICE_UNKNOWN",
	} {
		got, err := ex.Health(ctx, "", service)
		if err != nil || got != want {
			t.Errorf("Health(%q) = %q, %v; want %q", service, got, err, want)
		}
	}
}

func TestReflectionUnavailable(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer() // no reflection registered
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	ex := newExplorer(t, explorer.Options{})
	_, err = ex.ListServices(context.Background(), lis.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "does not support gRPC reflection") {
		t.Errorf("err = %v, want a reflection hint", err)
	}
}
