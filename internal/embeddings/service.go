package embeddings

import (
	"context"

	"google.golang.org/grpc"
)

// Service is the contract implemented by the embeddings worker
// binary. The daemon-side client invokes it transparently via
// gRPC ; the runtime/context_builder layer never sees this.
type Service interface {
	Embed(ctx context.Context, req *EmbedRequest) (*EmbedResponse, error)
	Rerank(ctx context.Context, req *RerankRequest) (*RerankResponse, error)
	Info(ctx context.Context, req *InfoRequest) (*InfoResponse, error)
}

// ServiceName is the gRPC service name on the wire.
const ServiceName = "digitorn.embeddings.v1.EmbeddingsService"

// Method names.
const (
	MethodEmbed  = "Embed"
	MethodRerank = "Rerank"
	MethodInfo   = "Info"
)

// RegisterService registers an EmbeddingsService impl on the gRPC
// server. The worker binary calls this in its Register hook.
func RegisterService(s *grpc.Server, impl Service) {
	s.RegisterService(&serviceDesc, impl)
}

var serviceDesc = grpc.ServiceDesc{
	ServiceName: ServiceName,
	HandlerType: (*Service)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: MethodEmbed, Handler: embedHandler},
		{MethodName: MethodRerank, Handler: rerankHandler},
		{MethodName: MethodInfo, Handler: infoHandler},
	},
	Metadata: "internal/embeddings/service.go",
}

func embedHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(EmbedRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(Service).Embed(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + ServiceName + "/" + MethodEmbed}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(Service).Embed(ctx, req.(*EmbedRequest))
	})
}

func rerankHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(RerankRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(Service).Rerank(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + ServiceName + "/" + MethodRerank}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(Service).Rerank(ctx, req.(*RerankRequest))
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

// CodecName mirrors llm's JSON codec — same encoding, simpler
// shared registration through the worker framework.
const CodecName = "json"

// Kind is the worker.Kind identifier for embeddings instances.
// The Manager spawns one binary per Spec, and routing keys off
// this constant.
const Kind = "embeddings"
