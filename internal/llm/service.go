package llm

import (
	"context"

	"google.golang.org/grpc"
)

// Service is the contract implemented by the worker binary. The client
// SDK calls it transparently via gRPC ; the runtime never sees this.
type Service interface {
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, req *ChatRequest, sink ChatStreamSink) error
	Embed(ctx context.Context, req *EmbedRequest) (*EmbedResponse, error)
	CountTokens(ctx context.Context, req *CountTokensRequest) (*CountTokensResponse, error)
	ListProviders(ctx context.Context, req *ListProvidersRequest) (*ListProvidersResponse, error)
	// Speak synthesizes req.Text to streamed audio frames (TTS) through the gateway.
	Speak(ctx context.Context, req *SpeechRequest, sink AudioSink) error
	// Transcribe turns an utterance's audio into streamed transcript frames (STT)
	// through the gateway.
	Transcribe(ctx context.Context, req *TranscribeRequest, sink AudioSink) error
}

// ChatStreamSink lets the worker push chunks back to the client. The
// underlying transport is a gRPC server-side stream.
type ChatStreamSink interface {
	Send(*ChatChunk) error
}

// AudioSink streams audio frames back to the client (Speak / Transcribe). The frames
// travel raw via the "digitorn.audio" codec — no base64, the latency-critical path.
type AudioSink interface {
	Send(*AudioFrame) error
}

// CountTokensRequest / CountTokensResponse — separate types so the wire
// schema is explicit (no untyped scalars).
type CountTokensRequest struct {
	Provider string        `json:"provider"`
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	APIKey   string        `json:"api_key,omitempty"`
	UserJWT  string        `json:"user_jwt,omitempty"`
}

type CountTokensResponse struct {
	Tokens uint64 `json:"tokens"`
	Model  string `json:"model"`
}

// ListProvidersRequest is intentionally empty — kept as a struct so the
// gRPC marshaler has something to unmarshal into.
type ListProvidersRequest struct{}

// ServiceName is the gRPC service name registered on the wire.
const ServiceName = "digitorn.llm.v1.LLMService"

// Method names.
const (
	MethodChat          = "Chat"
	MethodChatStream    = "ChatStream"
	MethodEmbed         = "Embed"
	MethodCountTokens   = "CountTokens"
	MethodListProviders = "ListProviders"
	MethodSpeak         = "Speak"
	MethodTranscribe    = "Transcribe"
)

// ----- Server-side wiring : grpc.ServiceDesc manuelle -----

// RegisterService registers an LLM Service implementation on a gRPC server.
// Called by the worker binary in its Register hook.
func RegisterService(s *grpc.Server, impl Service) {
	s.RegisterService(&serviceDesc, impl)
}

var serviceDesc = grpc.ServiceDesc{
	ServiceName: ServiceName,
	HandlerType: (*Service)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: MethodChat, Handler: chatHandler},
		{MethodName: MethodEmbed, Handler: embedHandler},
		{MethodName: MethodCountTokens, Handler: countTokensHandler},
		{MethodName: MethodListProviders, Handler: listProvidersHandler},
	},
	Streams: []grpc.StreamDesc{
		{StreamName: MethodChatStream, Handler: chatStreamHandler, ServerStreams: true},
		{StreamName: MethodSpeak, Handler: speakHandler, ServerStreams: true},
		{StreamName: MethodTranscribe, Handler: transcribeHandler, ServerStreams: true},
	},
	Metadata: "internal/llm/service.go",
}

func chatHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(ChatRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(Service).Chat(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + ServiceName + "/" + MethodChat}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(Service).Chat(ctx, req.(*ChatRequest))
	})
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

func countTokensHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(CountTokensRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(Service).CountTokens(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + ServiceName + "/" + MethodCountTokens}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(Service).CountTokens(ctx, req.(*CountTokensRequest))
	})
}

func listProvidersHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(ListProvidersRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(Service).ListProviders(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + ServiceName + "/" + MethodListProviders}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(Service).ListProviders(ctx, req.(*ListProvidersRequest))
	})
}

func chatStreamHandler(srv any, stream grpc.ServerStream) error {
	in := new(ChatRequest)
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	return srv.(Service).ChatStream(stream.Context(), in, &serverStreamSink{stream: stream})
}

type serverStreamSink struct{ stream grpc.ServerStream }

func (s *serverStreamSink) Send(c *ChatChunk) error { return s.stream.SendMsg(c) }

func speakHandler(srv any, stream grpc.ServerStream) error {
	in := new(SpeechRequest)
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	return srv.(Service).Speak(stream.Context(), in, &serverAudioSink{stream: stream})
}

func transcribeHandler(srv any, stream grpc.ServerStream) error {
	in := new(TranscribeRequest)
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	return srv.(Service).Transcribe(stream.Context(), in, &serverAudioSink{stream: stream})
}

type serverAudioSink struct{ stream grpc.ServerStream }

func (s *serverAudioSink) Send(f *AudioFrame) error { return s.stream.SendMsg(f) }
