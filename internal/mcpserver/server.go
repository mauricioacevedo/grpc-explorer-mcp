// Package mcpserver exposes an explorer.Explorer as a set of MCP tools.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauricioacevedo/grpc-explorer-mcp/internal/explorer"
)

const instructions = `Explore and call gRPC services through server reflection.
Typical flow: list_services -> describe_service -> describe_method (shows an example request) -> call_method.
Every tool takes an optional "target" (host:port); when omitted the server's default target is used.
call_method only works for unary methods and only when the server was started with --allow-calls.`

// New returns an MCP server exposing the explorer's capabilities as tools.
func New(ex *explorer.Explorer, version string) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "grpc-explorer-mcp", Title: "gRPC Explorer", Version: version},
		&mcp.ServerOptions{Instructions: instructions},
	)
	h := &handlers{ex: ex}

	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(true)}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_services",
		Title:       "List gRPC services",
		Description: "List the services a gRPC server exposes, discovered through server reflection.",
		Annotations: readOnly,
	}, h.listServices)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "describe_service",
		Title:       "Describe a gRPC service",
		Description: "List the methods of a service with their request/response types and whether they stream.",
		Annotations: readOnly,
	}, h.describeService)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "describe_method",
		Title:       "Describe a gRPC method",
		Description: "Show a method's request and response messages field by field, including an example JSON request body.",
		Annotations: readOnly,
	}, h.describeMethod)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "describe_message",
		Title:       "Describe a protobuf message",
		Description: "Show the fields of any message type known to the server, with an example JSON value.",
		Annotations: readOnly,
	}, h.describeMessage)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "health_check",
		Title:       "Check gRPC health",
		Description: "Query the standard grpc.health.v1 service. Leave service empty to check the whole server.",
		Annotations: readOnly,
	}, h.healthCheck)

	callDescription := "Call a unary gRPC method with a JSON request body (protobuf JSON mapping) and return the response and status code."
	if !ex.CallsAllowed() {
		callDescription += " Currently disabled: the server is running in read-only mode."
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "call_method",
		Title:       "Call a gRPC method",
		Description: callDescription,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, h.callMethod)

	return server
}

type handlers struct {
	ex *explorer.Explorer
}

// TargetInput is embedded by every tool input.
type TargetInput struct {
	Target string `json:"target,omitempty" jsonschema:"gRPC server address as host:port; defaults to the configured target"`
}

type ListServicesInput struct {
	TargetInput
}

type ListServicesOutput struct {
	Target   string   `json:"target"`
	Services []string `json:"services"`
}

func (h *handlers) listServices(ctx context.Context, _ *mcp.CallToolRequest, in ListServicesInput) (*mcp.CallToolResult, ListServicesOutput, error) {
	target, err := h.ex.ResolveTarget(in.Target)
	if err != nil {
		return nil, ListServicesOutput{}, err
	}
	services, err := h.ex.ListServices(ctx, target)
	if err != nil {
		return nil, ListServicesOutput{}, err
	}
	if services == nil {
		services = []string{}
	}
	return nil, ListServicesOutput{Target: target, Services: services}, nil
}

type DescribeServiceInput struct {
	TargetInput
	Service string `json:"service" jsonschema:"fully-qualified service name, e.g. shop.v1.OrderService"`
}

func (h *handlers) describeService(ctx context.Context, _ *mcp.CallToolRequest, in DescribeServiceInput) (*mcp.CallToolResult, explorer.ServiceInfo, error) {
	info, err := h.ex.DescribeService(ctx, in.Target, in.Service)
	return nil, info, err
}

type DescribeMethodInput struct {
	TargetInput
	Method string `json:"method" jsonschema:"method as package.Service/Method, e.g. shop.v1.OrderService/GetOrder"`
}

func (h *handlers) describeMethod(ctx context.Context, _ *mcp.CallToolRequest, in DescribeMethodInput) (*mcp.CallToolResult, explorer.MethodDetail, error) {
	detail, err := h.ex.DescribeMethod(ctx, in.Target, in.Method)
	return nil, detail, err
}

type DescribeMessageInput struct {
	TargetInput
	Message string `json:"message" jsonschema:"fully-qualified message name, e.g. shop.v1.Order"`
}

func (h *handlers) describeMessage(ctx context.Context, _ *mcp.CallToolRequest, in DescribeMessageInput) (*mcp.CallToolResult, explorer.MessageInfo, error) {
	info, err := h.ex.DescribeMessage(ctx, in.Target, in.Message)
	return nil, info, err
}

type HealthCheckInput struct {
	TargetInput
	Service string `json:"service,omitempty" jsonschema:"service to check; empty checks the whole server"`
}

type HealthCheckOutput struct {
	Target  string `json:"target"`
	Service string `json:"service"`
	Status  string `json:"status"`
}

func (h *handlers) healthCheck(ctx context.Context, _ *mcp.CallToolRequest, in HealthCheckInput) (*mcp.CallToolResult, HealthCheckOutput, error) {
	target, err := h.ex.ResolveTarget(in.Target)
	if err != nil {
		return nil, HealthCheckOutput{}, err
	}
	st, err := h.ex.Health(ctx, target, in.Service)
	if err != nil {
		return nil, HealthCheckOutput{}, err
	}
	return nil, HealthCheckOutput{Target: target, Service: in.Service, Status: st}, nil
}

type CallMethodInput struct {
	TargetInput
	Method   string            `json:"method" jsonschema:"unary method as package.Service/Method"`
	Request  map[string]any    `json:"request,omitempty" jsonschema:"request body in protobuf JSON form; see describe_method for an example"`
	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"extra gRPC metadata (headers) for this call"`
}

func (h *handlers) callMethod(ctx context.Context, _ *mcp.CallToolRequest, in CallMethodInput) (*mcp.CallToolResult, explorer.CallResult, error) {
	var body json.RawMessage
	if in.Request != nil {
		b, err := json.Marshal(in.Request)
		if err != nil {
			return nil, explorer.CallResult{}, fmt.Errorf("encoding request: %w", err)
		}
		body = b
	}
	result, err := h.ex.Call(ctx, in.Target, in.Method, body, in.Metadata)
	return nil, result, err
}

func ptr[T any](v T) *T { return &v }
