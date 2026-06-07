package service

import (
	"context"

	"google.golang.org/grpc"
)

// Service is the contract the worker binary implements and the daemon
// consumes via gRPC. Two methods, no stream variant in V1 :
//
//   - Invoke(ctx, req)   : synchronous tool dispatch
//   - Manifests(ctx, req): self-describe what the worker hosts
//
// Streaming variant (Invoke server-stream) is reserved for a future
// version when we migrate LLM into this generic pattern. For V1, LLM
// keeps its own internal/llm/service.go contract ; this one targets
// the SHELL / LSP / MCP / browser / OCR class of workers.
type Service interface {
	Invoke(ctx context.Context, req *InvokeRequest) (*InvokeResponse, error)
	Manifests(ctx context.Context, req *ManifestsRequest) (*ManifestsResponse, error)
}

// ServiceName is the fully-qualified gRPC service name on the wire.
// Versioned so V2 (e.g. protobuf migration) lives on a different path
// and old daemon ↔ new worker rejects cleanly with NOT_FOUND.
const ServiceName = "digitorn.module.v1.ModuleService"

// Method names — exported so the daemon-side client can build the
// fully-qualified RPC path "/{ServiceName}/{MethodInvoke}".
const (
	MethodInvoke    = "Invoke"
	MethodManifests = "Manifests"
	MethodTools     = "Tools"
)

// ToolsProvider is the optional extension reporting a module's runtime tools.
// The handler type-asserts it; an impl without it returns no dynamic tools.
type ToolsProvider interface {
	Tools(ctx context.Context, req *ToolsRequest) (*ToolsResponse, error)
}

// RegisterService wires a Service implementation onto a gRPC server.
// Called by the worker binary inside its worker.Run() Register hook.
func RegisterService(s *grpc.Server, impl Service) {
	s.RegisterService(&serviceDesc, impl)
}

// serviceDesc is the hand-written gRPC ServiceDesc. We avoid protoc
// (see codec.go for the rationale) so the descriptor lives here.
var serviceDesc = grpc.ServiceDesc{
	ServiceName: ServiceName,
	HandlerType: (*Service)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: MethodInvoke, Handler: invokeHandler},
		{MethodName: MethodManifests, Handler: manifestsHandler},
		{MethodName: MethodTools, Handler: toolsHandler},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "internal/module/service/service.go",
}

func invokeHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(InvokeRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(Service).Invoke(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + ServiceName + "/" + MethodInvoke}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(Service).Invoke(ctx, req.(*InvokeRequest))
	})
}

func manifestsHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(ManifestsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(Service).Manifests(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + ServiceName + "/" + MethodManifests}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(Service).Manifests(ctx, req.(*ManifestsRequest))
	})
}

func toolsHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(ToolsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	tp, ok := srv.(ToolsProvider)
	if !ok {
		return &ToolsResponse{ModuleID: in.ModuleID}, nil
	}
	if interceptor == nil {
		return tp.Tools(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + ServiceName + "/" + MethodTools}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return tp.Tools(ctx, req.(*ToolsRequest))
	})
}
