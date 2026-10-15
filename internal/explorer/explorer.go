// Package explorer discovers, describes and calls gRPC services through server
// reflection, enforcing the safety policy configured by the operator.
package explorer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/mauricioacevedo/grpc-explorer-mcp/internal/reflection"
)

// ErrCallsDisabled is returned by Call when the server runs in read-only mode.
var ErrCallsDisabled = errors.New("calling methods is disabled; restart grpc-explorer-mcp with --allow-calls to enable it")

// Options configure an Explorer.
type Options struct {
	// AllowedTargets are path.Match patterns for host:port targets.
	// Defaults to DefaultAllowedTargets.
	AllowedTargets []string
	// DefaultTarget is used when a request does not name a target.
	DefaultTarget string
	// AllowCalls enables Call. When false the explorer is read-only.
	AllowCalls bool
	// Timeout bounds every reflection request and call. Defaults to 10s.
	Timeout time.Duration
	// Headers are sent as gRPC metadata on every request (for example auth tokens).
	Headers map[string]string
	// TLS enables transport security using the system root CAs.
	TLS bool
	// TLSSkipVerify disables certificate verification. Only for development.
	TLSSkipVerify bool
	// MaxResponseBytes limits the size of a call's response. Defaults to 4 MiB.
	MaxResponseBytes int
	// UserAgent is sent to target servers.
	UserAgent string
}

// Explorer holds one client connection per target and serves exploration
// requests against them. It is safe for concurrent use.
type Explorer struct {
	opts   Options
	policy *TargetPolicy

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

// New validates opts and returns an Explorer.
func New(opts Options) (*Explorer, error) {
	if len(opts.AllowedTargets) == 0 {
		opts.AllowedTargets = DefaultAllowedTargets
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = 4 << 20
	}
	policy, err := NewTargetPolicy(opts.AllowedTargets)
	if err != nil {
		return nil, err
	}
	if opts.DefaultTarget != "" {
		if err := policy.Check(opts.DefaultTarget); err != nil {
			return nil, fmt.Errorf("default target: %w", err)
		}
	}
	return &Explorer{opts: opts, policy: policy, conns: map[string]*grpc.ClientConn{}}, nil
}

// CallsAllowed reports whether Call is enabled.
func (e *Explorer) CallsAllowed() bool { return e.opts.AllowCalls }

// Close closes every open connection.
func (e *Explorer) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var errs []error
	for target, conn := range e.conns {
		errs = append(errs, conn.Close())
		delete(e.conns, target)
	}
	return errors.Join(errs...)
}

// ResolveTarget returns target, or the default target when target is empty,
// after checking it against the policy.
func (e *Explorer) ResolveTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		target = e.opts.DefaultTarget
	}
	if target == "" {
		return "", errors.New("no target given and no default target configured; pass a host:port target")
	}
	if err := e.policy.Check(target); err != nil {
		return "", err
	}
	return target, nil
}

// ListServices returns the services exposed by target.
func (e *Explorer) ListServices(ctx context.Context, target string) ([]string, error) {
	ctx, cancel, conn, err := e.prepare(ctx, target)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return reflection.NewClient(conn).ListServices(ctx)
}

// DescribeService returns the methods of service on target.
func (e *Explorer) DescribeService(ctx context.Context, target, service string) (ServiceInfo, error) {
	ctx, cancel, conn, err := e.prepare(ctx, target)
	if err != nil {
		return ServiceInfo{}, err
	}
	defer cancel()
	sd, _, err := findService(ctx, conn, service)
	if err != nil {
		return ServiceInfo{}, err
	}
	return describeService(sd), nil
}

// MethodDetail describes a method together with its request and response messages.
type MethodDetail struct {
	Method   MethodInfo  `json:"method"`
	Request  MessageInfo `json:"request"`
	Response MessageInfo `json:"response"`
}

// DescribeMethod returns a method and the shape of its request and response.
func (e *Explorer) DescribeMethod(ctx context.Context, target, method string) (MethodDetail, error) {
	ctx, cancel, conn, err := e.prepare(ctx, target)
	if err != nil {
		return MethodDetail{}, err
	}
	defer cancel()
	md, _, err := findMethod(ctx, conn, method)
	if err != nil {
		return MethodDetail{}, err
	}
	return MethodDetail{
		Method:   describeMethod(md),
		Request:  describeMessage(md.Input()),
		Response: describeMessage(md.Output()),
	}, nil
}

// DescribeMessage returns the fields of a message type and an example value.
func (e *Explorer) DescribeMessage(ctx context.Context, target, message string) (MessageInfo, error) {
	ctx, cancel, conn, err := e.prepare(ctx, target)
	if err != nil {
		return MessageInfo{}, err
	}
	defer cancel()
	message = strings.TrimPrefix(strings.TrimSpace(message), ".")
	files, err := reflection.NewClient(conn).FilesForSymbol(ctx, message)
	if err != nil {
		return MessageInfo{}, fmt.Errorf("looking up message %q: %w", message, err)
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName(message))
	if err != nil {
		return MessageInfo{}, fmt.Errorf("message %q not found: %w", message, err)
	}
	msg, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return MessageInfo{}, fmt.Errorf("%q is not a message type", message)
	}
	return describeMessage(msg), nil
}

// CallResult is the outcome of a unary call. A non-OK gRPC status is reported
// in Code and Message rather than as an error, so the caller can reason about it.
type CallResult struct {
	Method     string              `json:"method"`
	Code       string              `json:"code"`
	Message    string              `json:"message,omitempty"`
	Response   any                 `json:"response,omitempty"`
	Headers    map[string][]string `json:"headers,omitempty"`
	DurationMs int64               `json:"duration_ms"`
}

// Call invokes a unary method with a JSON request body (protojson format).
// extra metadata is merged over the configured headers.
func (e *Explorer) Call(ctx context.Context, target, method string, request json.RawMessage, extra map[string]string) (CallResult, error) {
	if !e.opts.AllowCalls {
		return CallResult{}, ErrCallsDisabled
	}
	ctx, cancel, conn, err := e.prepare(ctx, target)
	if err != nil {
		return CallResult{}, err
	}
	defer cancel()

	md, files, err := findMethod(ctx, conn, method)
	if err != nil {
		return CallResult{}, err
	}
	if md.IsStreamingClient() || md.IsStreamingServer() {
		return CallResult{}, fmt.Errorf("%s is a streaming method; only unary methods can be called", describeMethod(md).FullName)
	}

	types := dynamicpb.NewTypes(files)
	req := dynamicpb.NewMessage(md.Input())
	if len(request) > 0 && string(request) != "null" {
		if err := (protojson.UnmarshalOptions{Resolver: types}).Unmarshal(request, req); err != nil {
			return CallResult{}, fmt.Errorf("request does not match %s (use describe_method to see the expected shape): %w", md.Input().FullName(), err)
		}
	}
	for k, v := range extra {
		ctx = metadata.AppendToOutgoingContext(ctx, k, v)
	}

	fullName := describeMethod(md).FullName
	resp := dynamicpb.NewMessage(md.Output())
	var header metadata.MD
	start := time.Now()
	err = conn.Invoke(ctx, "/"+fullName, req, resp,
		grpc.Header(&header),
		grpc.MaxCallRecvMsgSize(e.opts.MaxResponseBytes),
	)
	result := CallResult{
		Method:     fullName,
		DurationMs: time.Since(start).Milliseconds(),
		Headers:    header,
	}
	if err != nil {
		st := status.Convert(err)
		result.Code = st.Code().String()
		result.Message = st.Message()
		return result, nil
	}
	result.Code = codes.OK.String()

	body, err := (protojson.MarshalOptions{Resolver: types}).Marshal(resp)
	if err != nil {
		return CallResult{}, fmt.Errorf("encoding response: %w", err)
	}
	if err := json.Unmarshal(body, &result.Response); err != nil {
		return CallResult{}, fmt.Errorf("decoding response: %w", err)
	}
	return result, nil
}

// Health checks the standard grpc.health.v1 service. An empty service checks
// the server as a whole.
func (e *Explorer) Health(ctx context.Context, target, service string) (string, error) {
	ctx, cancel, conn, err := e.prepare(ctx, target)
	if err != nil {
		return "", err
	}
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: service})
	switch status.Code(err) {
	case codes.OK:
		return resp.GetStatus().String(), nil
	case codes.Unimplemented:
		return "", errors.New("server does not implement the grpc.health.v1.Health service")
	case codes.NotFound:
		return "SERVICE_UNKNOWN", nil
	default:
		return "", err
	}
}

// prepare resolves and authorizes the target, applies the timeout and
// configured headers, and returns a connection.
func (e *Explorer) prepare(ctx context.Context, target string) (context.Context, context.CancelFunc, *grpc.ClientConn, error) {
	target, err := e.ResolveTarget(target)
	if err != nil {
		return nil, nil, nil, err
	}
	conn, err := e.conn(target)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, e.opts.Timeout)
	for k, v := range e.opts.Headers {
		ctx = metadata.AppendToOutgoingContext(ctx, k, v)
	}
	return ctx, cancel, conn, nil
}

func (e *Explorer) conn(target string) (*grpc.ClientConn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if conn, ok := e.conns[target]; ok {
		return conn, nil
	}
	creds := insecure.NewCredentials()
	if e.opts.TLS {
		creds = credentials.NewTLS(&tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: e.opts.TLSSkipVerify, //nolint:gosec // opt-in flag for development servers
		})
	}
	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if e.opts.UserAgent != "" {
		dialOpts = append(dialOpts, grpc.WithUserAgent(e.opts.UserAgent))
	}
	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", target, err)
	}
	e.conns[target] = conn
	return conn, nil
}

func findService(ctx context.Context, conn grpc.ClientConnInterface, service string) (protoreflect.ServiceDescriptor, *protoregistry.Files, error) {
	service = strings.TrimPrefix(strings.TrimSpace(service), ".")
	files, err := reflection.NewClient(conn).FilesForSymbol(ctx, service)
	if err != nil {
		return nil, nil, fmt.Errorf("looking up service %q: %w", service, err)
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName(service))
	if err != nil {
		return nil, nil, fmt.Errorf("service %q not found: %w", service, err)
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, nil, fmt.Errorf("%q is not a service", service)
	}
	return sd, files, nil
}

// findMethod accepts "pkg.Service/Method", "/pkg.Service/Method" or
// "pkg.Service.Method".
func findMethod(ctx context.Context, conn grpc.ClientConnInterface, method string) (protoreflect.MethodDescriptor, *protoregistry.Files, error) {
	service, name, err := splitMethod(method)
	if err != nil {
		return nil, nil, err
	}
	sd, files, err := findService(ctx, conn, service)
	if err != nil {
		return nil, nil, err
	}
	md := sd.Methods().ByName(protoreflect.Name(name))
	if md == nil {
		return nil, nil, fmt.Errorf("method %q not found in service %s", name, sd.FullName())
	}
	return md, files, nil
}

func splitMethod(method string) (service, name string, err error) {
	m := strings.TrimPrefix(strings.TrimSpace(method), "/")
	i := strings.LastIndex(m, "/")
	if i < 0 {
		i = strings.LastIndex(m, ".")
	}
	if i <= 0 || i == len(m)-1 {
		return "", "", fmt.Errorf("method %q must look like package.Service/Method", method)
	}
	return m[:i], m[i+1:], nil
}
