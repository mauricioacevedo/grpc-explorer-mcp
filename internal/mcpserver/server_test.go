package mcpserver_test

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"

	"github.com/mauricioacevedo/grpc-explorer-mcp/internal/demo"
	"github.com/mauricioacevedo/grpc-explorer-mcp/internal/explorer"
	"github.com/mauricioacevedo/grpc-explorer-mcp/internal/mcpserver"
)

// connect starts the demo gRPC server and returns an MCP client session wired
// to grpc-explorer-mcp through in-memory transports.
func connect(t *testing.T, allowCalls bool) *mcp.ClientSession {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	demo.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	ex, err := explorer.New(explorer.Options{DefaultTarget: lis.Addr().String(), AllowCalls: allowCalls})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ex.Close() })

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := mcpserver.New(ex, "test").Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var out map[string]any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, &out)
	}
	return res, out
}

func text(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestToolsAreListedWithAnnotations(t *testing.T) {
	s := connect(t, false)
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Name == "call_method" {
			if tool.Annotations.ReadOnlyHint {
				t.Error("call_method must not be marked read-only")
			}
			if !strings.Contains(tool.Description, "read-only mode") {
				t.Errorf("call_method description should mention read-only mode: %q", tool.Description)
			}
		} else if !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s should be marked read-only", tool.Name)
		}
	}
	slices.Sort(names)
	want := []string{"call_method", "describe_message", "describe_method", "describe_service", "health_check", "list_services"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

func TestExploreAndCallFlow(t *testing.T) {
	s := connect(t, true)

	res, out := call(t, s, "list_services", nil)
	if res.IsError {
		t.Fatalf("list_services failed: %s", text(res))
	}
	if !strings.Contains(text(res), "shop.v1.OrderService") || out["target"] == "" {
		t.Errorf("list_services = %s", text(res))
	}

	res, out = call(t, s, "describe_method", map[string]any{"method": "shop.v1.InventoryService/GetStock"})
	if res.IsError {
		t.Fatalf("describe_method failed: %s", text(res))
	}
	request := out["request"].(map[string]any)
	if request["name"] != "shop.v1.GetStockRequest" {
		t.Errorf("request = %v", request)
	}

	res, out = call(t, s, "call_method", map[string]any{
		"method":  "shop.v1.InventoryService/GetStock",
		"request": map[string]any{"sku": "KB-01"},
	})
	if res.IsError {
		t.Fatalf("call_method failed: %s", text(res))
	}
	stock := out["response"].(map[string]any)
	if out["code"] != "OK" || stock["available"] != float64(14) || stock["warehouse"] != "MDE-1" {
		t.Errorf("call_method = %v", out)
	}

	res, out = call(t, s, "health_check", map[string]any{"service": "shop.v1.InventoryService"})
	if res.IsError || out["status"] != "SERVING" {
		t.Errorf("health_check = %s", text(res))
	}
}

func TestErrorsAreReturnedToTheModel(t *testing.T) {
	s := connect(t, false)

	res, _ := call(t, s, "call_method", map[string]any{"method": "shop.v1.OrderService/GetOrder"})
	if !res.IsError || !strings.Contains(text(res), "--allow-calls") {
		t.Errorf("read-only call_method = %v %s", res.IsError, text(res))
	}

	res, _ = call(t, s, "list_services", map[string]any{"target": "example.com:443"})
	if !res.IsError || !strings.Contains(text(res), "not allowed") {
		t.Errorf("disallowed target = %v %s", res.IsError, text(res))
	}
}
