package tokenizer

import (
	"context"

	"google.golang.org/grpc"
)

// Service is the contract implemented by the tokenizer worker binary. The
// daemon-side Client invokes it transparently via gRPC.
type Service interface {
	Count(ctx context.Context, req *CountRequest) (*CountResponse, error)
	Info(ctx context.Context, req *InfoRequest) (*InfoResponse, error)
}

// ServiceName is the gRPC service name on the wire.
const ServiceName = "digitorn.tokenizer.v1.TokenizerService"

// Method names.
const (
	MethodCount = "Count"
	MethodInfo  = "Info"
)

// CodecName is the gRPC content-subtype for tokenizer RPCs. It has its OWN
// name (not the bare "json" that llm registers, nor "json+module") so importing
// this package self-registers the codec without ever colliding with another
// service's — see codec.go.
const CodecName = "json+tokenizer"

// Kind is the worker.Kind identifier for tokenizer instances.
const Kind = "tokenizer"

// RegisterService registers a tokenizer Service impl on the gRPC server. The
// worker binary calls this in its Register hook.
func RegisterService(s *grpc.Server, impl Service) {
	s.RegisterService(&serviceDesc, impl)
}

var serviceDesc = grpc.ServiceDesc{
	ServiceName: ServiceName,
	HandlerType: (*Service)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: MethodCount, Handler: countHandler},
		{MethodName: MethodInfo, Handler: infoHandler},
	},
	Metadata: "internal/tokenizer/service.go",
}

func countHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(CountRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(Service).Count(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + ServiceName + "/" + MethodCount}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(Service).Count(ctx, req.(*CountRequest))
	})
}

func infoHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(InfoRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(Service).Info(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + ServiceName + "/" + MethodInfo}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(Service).Info(ctx, req.(*InfoRequest))
	})
}
