package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/digitornai/digitorn/internal/worker"
)

// htrace writes a debug trace line to stderr when DIGITORN_LLM_TRACE is set,
// and is a no-op otherwise (cheap env check, no allocation). Used to diagnose
// the streaming / pick path without polluting the structured logs.
func htrace(format string, args ...any) {
	if os.Getenv("DIGITORN_LLM_TRACE") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "[llm-trace] "+format+"\n", args...)
}

// Client is the public face of the LLM provider module that the runtime
// (and any other in-process caller) consumes. It hides ALL of :
//   - gRPC marshalling
//   - worker subprocess lifecycle
//   - load-balancing between worker instances
//   - retry on transient failures
//   - cancellation propagation
//
// The runtime calls a Go method and gets Go types. Future migration to
// protobuf wire format / different transport / in-process collocation
// is a non-breaking change.
type Client struct {
	mgr     *worker.Manager
	kind    worker.Kind
	log     *slog.Logger
	retries int
	timeout time.Duration
}

// ClientConfig configures a Client.
type ClientConfig struct {
	Manager *worker.Manager // required
	Kind    worker.Kind     // default "llm"
	Retries int             // default 1 (= one retry on transient failure)
	Timeout time.Duration   // default 60s — wall-clock cap for one call
	Logger  *slog.Logger    // default slog.Default
}

// NewClient builds a Client. It does NOT spawn workers — the caller must
// already have Manager.Spawn'd the workers of the given Kind.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Manager == nil {
		return nil, errors.New("llm: client manager required")
	}
	if cfg.Kind == "" {
		cfg.Kind = "llm"
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	if cfg.Retries == 0 {
		cfg.Retries = 1
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Client{
		mgr: cfg.Manager, kind: cfg.Kind, log: cfg.Logger,
		retries: cfg.Retries, timeout: cfg.Timeout,
	}, nil
}

// Chat performs a single non-streaming chat completion.
func (c *Client) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	if req == nil {
		return nil, errors.New("llm: nil request")
	}
	ctx, cancel := c.withTimeout(ctx, req.Timeout)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			// Backoff before every re-attempt (never retry instantly).
			// Abort the wait if the caller's context is already gone.
			select {
			case <-time.After(retryBackoff(attempt)):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		conn, err := c.mgr.Pick(ctx, c.kind)
		if err != nil {
			lastErr = err
			if !isRetryable(err) {
				return nil, err
			}
			continue
		}
		out := new(ChatResponse)
		err = conn.GRPC().Invoke(ctx,
			"/"+ServiceName+"/"+MethodChat,
			req, out,
			grpc.CallContentSubtype(CodecName),
		)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !isRetryable(err) {
			return nil, err
		}
		c.log.Debug("llm.Chat retry",
			slog.Int("attempt", attempt+1),
			slog.String("err", err.Error()))
	}
	return nil, fmt.Errorf("llm: chat failed after %d attempts: %w", c.retries+1, lastErr)
}

// ChatStream initiates a streaming chat completion. The returned channel
// receives chunks as they arrive and is closed when the stream ends
// (EOF, error, or context cancellation). The caller MUST drain the
// channel to release the underlying goroutine ; cancelling ctx is the
// safe way to abort early.
func (c *Client) ChatStream(ctx context.Context, req *ChatRequest) (<-chan *ChatChunk, error) {
	if req == nil {
		return nil, errors.New("llm: nil request")
	}
	// Note : the call's timeout governs the *whole stream*. We do NOT
	// apply c.timeout here ; long generations can exceed 60s legitimately.
	htrace("CLIENT ChatStream Pick...")
	conn, err := c.mgr.Pick(ctx, c.kind)
	if err != nil {
		return nil, err
	}
	htrace("CLIENT Pick ok, NewStream...")

	desc := &grpc.StreamDesc{StreamName: MethodChatStream, ServerStreams: true}
	stream, err := conn.GRPC().NewStream(ctx, desc,
		"/"+ServiceName+"/"+MethodChatStream,
		grpc.CallContentSubtype(CodecName),
	)
	if err != nil {
		return nil, fmt.Errorf("llm: stream open: %w", err)
	}
	htrace("CLIENT NewStream ok, SendMsg...")
	if err := stream.SendMsg(req); err != nil {
		return nil, fmt.Errorf("llm: stream send: %w", err)
	}
	htrace("CLIENT SendMsg ok")
	if err := stream.CloseSend(); err != nil {
		return nil, fmt.Errorf("llm: stream close-send: %w", err)
	}

	out := make(chan *ChatChunk, 32)
	go func() {
		defer close(out)
		for {
			chunk := new(ChatChunk)
			err := stream.RecvMsg(chunk)
			if err == io.EOF {
				return
			}
			if err != nil {
				select {
				case out <- &ChatChunk{Error: err.Error()}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Speak streams synthesized audio for req.Text. The returned channel receives raw
// AudioFrame messages (FrameAudio … FrameDone) as they arrive and is closed at end
// of stream. The caller MUST drain it; cancelling ctx aborts early (barge-in). The
// audio travels via the raw "digitorn.audio" codec — no base64 on the hot path.
func (c *Client) Speak(ctx context.Context, req *SpeechRequest) (<-chan *AudioFrame, error) {
	if req == nil {
		return nil, errors.New("llm: nil request")
	}
	conn, err := c.mgr.Pick(ctx, c.kind)
	if err != nil {
		return nil, err
	}
	desc := &grpc.StreamDesc{StreamName: MethodSpeak, ServerStreams: true}
	stream, err := conn.GRPC().NewStream(ctx, desc,
		"/"+ServiceName+"/"+MethodSpeak,
		grpc.CallContentSubtype(AudioCodecName),
	)
	if err != nil {
		return nil, fmt.Errorf("llm: speak open: %w", err)
	}
	if err := stream.SendMsg(req); err != nil {
		return nil, fmt.Errorf("llm: speak send: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return nil, fmt.Errorf("llm: speak close-send: %w", err)
	}
	out := make(chan *AudioFrame, 64)
	go func() {
		defer close(out)
		for {
			f := new(AudioFrame)
			err := stream.RecvMsg(f)
			if err == io.EOF {
				return
			}
			if err != nil {
				select {
				case out <- ErrorFrame(err.Error()):
				case <-ctx.Done():
				}
				return
			}
			select {
			case out <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Transcribe streams transcript frames (FrameText interim … FrameFinal … FrameDone)
// for the utterance audio in req.Audio. Drain-safe; cancelling ctx aborts early.
func (c *Client) Transcribe(ctx context.Context, req *TranscribeRequest) (<-chan *AudioFrame, error) {
	if req == nil {
		return nil, errors.New("llm: nil request")
	}
	conn, err := c.mgr.Pick(ctx, c.kind)
	if err != nil {
		return nil, err
	}
	desc := &grpc.StreamDesc{StreamName: MethodTranscribe, ServerStreams: true}
	stream, err := conn.GRPC().NewStream(ctx, desc,
		"/"+ServiceName+"/"+MethodTranscribe,
		grpc.CallContentSubtype(AudioCodecName),
	)
	if err != nil {
		return nil, fmt.Errorf("llm: transcribe open: %w", err)
	}
	if err := stream.SendMsg(req); err != nil {
		return nil, fmt.Errorf("llm: transcribe send: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return nil, fmt.Errorf("llm: transcribe close-send: %w", err)
	}
	out := make(chan *AudioFrame, 16)
	go func() {
		defer close(out)
		for {
			f := new(AudioFrame)
			err := stream.RecvMsg(f)
			if err == io.EOF {
				return
			}
			if err != nil {
				select {
				case out <- ErrorFrame(err.Error()):
				case <-ctx.Done():
				}
				return
			}
			select {
			case out <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// TranscribeText is the unary form of Transcribe for single-utterance
// (batch) callers like the HTTP /api/transcribe endpoint. It drains the
// stream to the final transcript and, crucially, returns the upstream
// gRPC error UNFLATTENED — the streaming Transcribe collapses errors to
// an ErrorFrame string (fine for the voice pipeline, but it discards the
// status details a caller needs to tell "quota_exceeded" from a transient
// provider failure). Here the raw *status.Status propagates so the HTTP
// layer can classify it (see server.classifyLLMError).
func (c *Client) TranscribeText(ctx context.Context, req *TranscribeRequest) (string, error) {
	if req == nil {
		return "", errors.New("llm: nil request")
	}
	conn, err := c.mgr.Pick(ctx, c.kind)
	if err != nil {
		return "", err
	}
	desc := &grpc.StreamDesc{StreamName: MethodTranscribe, ServerStreams: true}
	stream, err := conn.GRPC().NewStream(ctx, desc,
		"/"+ServiceName+"/"+MethodTranscribe,
		grpc.CallContentSubtype(AudioCodecName),
	)
	if err != nil {
		return "", fmt.Errorf("llm: transcribe open: %w", err)
	}
	if err := stream.SendMsg(req); err != nil {
		return "", fmt.Errorf("llm: transcribe send: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return "", fmt.Errorf("llm: transcribe close-send: %w", err)
	}
	var final, interim string
	for {
		f := new(AudioFrame)
		err := stream.RecvMsg(f)
		if err == io.EOF {
			break
		}
		if err != nil {
			// Return the gRPC status verbatim — its ErrorInfo detail
			// carries upstream_code (e.g. "quota_exceeded").
			return "", err
		}
		switch f.Kind() {
		case FrameFinal:
			if final != "" {
				final += " "
			}
			final += f.Text()
		case FrameText:
			interim = f.Text()
		}
	}
	if final != "" {
		return final, nil
	}
	return interim, nil
}

// Embed computes embeddings for the input texts.
func (c *Client) Embed(ctx context.Context, req *EmbedRequest) (*EmbedResponse, error) {
	if req == nil {
		return nil, errors.New("llm: nil request")
	}
	ctx, cancel := c.withTimeout(ctx, req.Timeout)
	defer cancel()
	conn, err := c.mgr.Pick(ctx, c.kind)
	if err != nil {
		return nil, err
	}
	out := new(EmbedResponse)
	err = conn.GRPC().Invoke(ctx,
		"/"+ServiceName+"/"+MethodEmbed,
		req, out,
		grpc.CallContentSubtype(CodecName),
	)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CountTokens estimates the token count for a message set.
func (c *Client) CountTokens(ctx context.Context, req *CountTokensRequest) (*CountTokensResponse, error) {
	if req == nil {
		return nil, errors.New("llm: nil request")
	}
	ctx, cancel := c.withTimeout(ctx, 0)
	defer cancel()
	conn, err := c.mgr.Pick(ctx, c.kind)
	if err != nil {
		return nil, err
	}
	out := new(CountTokensResponse)
	err = conn.GRPC().Invoke(ctx,
		"/"+ServiceName+"/"+MethodCountTokens,
		req, out,
		grpc.CallContentSubtype(CodecName),
	)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListProviders enumerates what the worker offers.
func (c *Client) ListProviders(ctx context.Context) (*ListProvidersResponse, error) {
	ctx, cancel := c.withTimeout(ctx, 0)
	defer cancel()
	conn, err := c.mgr.Pick(ctx, c.kind)
	if err != nil {
		return nil, err
	}
	out := new(ListProvidersResponse)
	err = conn.GRPC().Invoke(ctx,
		"/"+ServiceName+"/"+MethodListProviders,
		new(ListProvidersRequest), out,
		grpc.CallContentSubtype(CodecName),
	)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Stats exposes the underlying worker pool's stats for /diagnostics.
type Stats struct {
	WorkerPool worker.PoolStats
}

func (c *Client) Stats() Stats {
	st := c.mgr.Stats()
	return Stats{WorkerPool: st.Pools[c.kind]}
}

func (c *Client) withTimeout(ctx context.Context, override time.Duration) (context.Context, context.CancelFunc) {
	if override > 0 {
		return context.WithTimeout(ctx, override)
	}
	return context.WithTimeout(ctx, c.timeout)
}

// isRetryable returns true if the gRPC error is one we should retry once
// (transient network / unavailability + upstream rate-limit). Application-
// level errors (invalid API key, malformed request) are NOT retried.
//
// Phase 1 extension: bifrost/service.go::translateError now maps HTTP
// status codes to proper gRPC codes (previously every error was
// codes.Unknown and NOTHING retried). The whitelist below picks up the
// new mappings:
//
//   - Unavailable        → 500/502/503/504 + transport drops (retry)
//   - DeadlineExceeded   → 408 + ctx-timeout (retry)
//   - ResourceExhausted  → 429 explicit upstream rate-limit (retry once)
//   - Aborted            → 409 concurrent-write conflict (retry once)
//
// InvalidArgument (400), Unauthenticated (401), PermissionDenied (403),
// NotFound (404), Unimplemented (501) stay non-retriable: re-issuing the
// same request would deterministically fail again, burning quota.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, worker.ErrNoHealthyWorker) {
		return true
	}
	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.Unavailable,
			codes.DeadlineExceeded,
			codes.Aborted:
			return true
		case codes.ResourceExhausted:
			// A quota block (our own limit) must never be retried; only a
			// genuine upstream rate-limit is.
			return upstreamCodeOf(s) != "quota_exceeded"
		}
	}
	return false
}

// upstreamCodeOf reads the gateway's error.code from the ErrorInfo detail.
func upstreamCodeOf(s *status.Status) string {
	for _, d := range s.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.Metadata["upstream_code"]
		}
	}
	return ""
}

// retryBackoff returns the delay before retry attempt N (1-based):
// 200ms, 400ms, 800ms … capped at 5s. Retrying a rate-limited or
// unavailable upstream with ZERO delay (the previous behaviour) just
// hammers it; a short exponential backoff is the correct response to
// 429/503.
func retryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Cap the shift BEFORE computing: 200ms << 5 = 6.4s already exceeds
	// the 5s ceiling, and a large attempt would overflow int64 and wrap
	// to a bogus (even negative) duration. Clamp the exponent, then the
	// value.
	shift := attempt - 1
	if shift > 5 {
		shift = 5
	}
	d := 200 * time.Millisecond << shift
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}
