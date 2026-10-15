// Package reflection is a minimal client for the gRPC server reflection
// protocol. It lists services and rebuilds protobuf descriptors at runtime, so
// a server can be explored and called without its .proto files.
//
// Both reflection versions are supported: grpc.reflection.v1 is tried first and
// grpc.reflection.v1alpha is used as a fallback for older servers. The two
// versions are wire-compatible, so v1alpha messages are converted by
// re-marshaling them.
package reflection

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	rpbalpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha" //nolint:staticcheck // see exchangeV1Alpha
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// reflectionServices are the names servers report for the reflection service
// itself; they are hidden from service listings.
var reflectionServices = map[string]bool{
	"grpc.reflection.v1.ServerReflection":      true,
	"grpc.reflection.v1alpha.ServerReflection": true,
}

// Client queries a single gRPC server through its reflection service.
type Client struct {
	conn grpc.ClientConnInterface
}

// NewClient returns a reflection client that uses conn.
func NewClient(conn grpc.ClientConnInterface) *Client {
	return &Client{conn: conn}
}

// ListServices returns the fully-qualified names of the services exposed by
// the server, sorted, excluding the reflection service itself.
func (c *Client) ListServices(ctx context.Context) ([]string, error) {
	resp, err := c.exchange(ctx, &rpb.ServerReflectionRequest{
		MessageRequest: &rpb.ServerReflectionRequest_ListServices{ListServices: "*"},
	})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, svc := range resp.GetListServicesResponse().GetService() {
		if !reflectionServices[svc.GetName()] {
			names = append(names, svc.GetName())
		}
	}
	sort.Strings(names)
	return names, nil
}

// FilesForSymbol returns a registry holding the file that defines symbol (a
// fully-qualified service, method, message or enum name) and all of its
// transitive dependencies.
func (c *Client) FilesForSymbol(ctx context.Context, symbol string) (*protoregistry.Files, error) {
	resp, err := c.exchange(ctx, &rpb.ServerReflectionRequest{
		MessageRequest: &rpb.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: symbol},
	})
	if err != nil {
		return nil, err
	}

	files := map[string]*descriptorpb.FileDescriptorProto{}
	if err := addFiles(files, resp); err != nil {
		return nil, err
	}

	// Servers usually send the full dependency closure, but the protocol
	// allows them to omit files the client is expected to already have.
	// Resolve anything missing from the locally linked well-known types
	// first, and ask the server otherwise.
	for {
		missing := missingDeps(files)
		if len(missing) == 0 {
			break
		}
		for _, name := range missing {
			if fd, err := protoregistry.GlobalFiles.FindFileByPath(name); err == nil {
				files[name] = protodesc.ToFileDescriptorProto(fd)
				continue
			}
			resp, err := c.exchange(ctx, &rpb.ServerReflectionRequest{
				MessageRequest: &rpb.ServerReflectionRequest_FileByFilename{FileByFilename: name},
			})
			if err != nil {
				return nil, fmt.Errorf("fetching dependency %q: %w", name, err)
			}
			if err := addFiles(files, resp); err != nil {
				return nil, err
			}
			if _, ok := files[name]; !ok {
				return nil, fmt.Errorf("server did not return dependency %q", name)
			}
		}
	}

	set := &descriptorpb.FileDescriptorSet{}
	for _, fd := range files {
		set.File = append(set.File, fd)
	}
	registry, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("building descriptors for %q: %w", symbol, err)
	}
	return registry, nil
}

func addFiles(files map[string]*descriptorpb.FileDescriptorProto, resp *rpb.ServerReflectionResponse) error {
	for _, raw := range resp.GetFileDescriptorResponse().GetFileDescriptorProto() {
		fd := &descriptorpb.FileDescriptorProto{}
		if err := proto.Unmarshal(raw, fd); err != nil {
			return fmt.Errorf("decoding file descriptor: %w", err)
		}
		files[fd.GetName()] = fd
	}
	return nil
}

func missingDeps(files map[string]*descriptorpb.FileDescriptorProto) []string {
	seen := map[string]bool{}
	var missing []string
	for _, fd := range files {
		for _, dep := range fd.GetDependency() {
			if _, ok := files[dep]; !ok && !seen[dep] {
				seen[dep] = true
				missing = append(missing, dep)
			}
		}
	}
	sort.Strings(missing)
	return missing
}

// exchange sends one request and returns its response, preferring reflection
// v1 and falling back to v1alpha when the server does not implement v1.
func (c *Client) exchange(ctx context.Context, req *rpb.ServerReflectionRequest) (*rpb.ServerReflectionResponse, error) {
	resp, err := c.exchangeV1(ctx, req)
	if status.Code(err) == codes.Unimplemented {
		resp, err = c.exchangeV1Alpha(ctx, req)
	}
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return nil, errors.New("server does not support gRPC reflection; enable it with reflection.Register (Go) or the equivalent for your language")
		}
		return nil, err
	}
	if e := resp.GetErrorResponse(); e != nil {
		code := codes.Unknown
		if c := e.GetErrorCode(); c >= 0 {
			code = codes.Code(uint32(c)) //nolint:gosec // c is non-negative
		}
		return nil, status.Error(code, e.GetErrorMessage())
	}
	return resp, nil
}

func (c *Client) exchangeV1(ctx context.Context, req *rpb.ServerReflectionRequest) (*rpb.ServerReflectionResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := rpb.NewServerReflectionClient(c.conn).ServerReflectionInfo(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(req); err != nil {
		return nil, recvStatus(err, stream.Recv)
	}
	resp, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	_ = stream.CloseSend()
	return resp, nil
}

//nolint:staticcheck // v1alpha is deprecated, but many servers still only implement it.
func (c *Client) exchangeV1Alpha(ctx context.Context, req *rpb.ServerReflectionRequest) (*rpb.ServerReflectionResponse, error) {
	alphaReq := &rpbalpha.ServerReflectionRequest{}
	if err := convert(req, alphaReq); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := rpbalpha.NewServerReflectionClient(c.conn).ServerReflectionInfo(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(alphaReq); err != nil {
		return nil, recvStatus(err, stream.Recv)
	}
	alphaResp, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	_ = stream.CloseSend()

	resp := &rpb.ServerReflectionResponse{}
	if err := convert(alphaResp, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// recvStatus handles a failed Send. gRPC reports io.EOF from Send when the
// server has already ended the stream (for example with Unimplemented); the
// actual status is only available from Recv.
func recvStatus[T any](sendErr error, recv func() (T, error)) error {
	if !errors.Is(sendErr, io.EOF) {
		return sendErr
	}
	if _, err := recv(); err != nil {
		return err
	}
	return sendErr
}

func convert(from, to proto.Message) error {
	b, err := proto.Marshal(from)
	if err != nil {
		return err
	}
	return proto.Unmarshal(b, to)
}
