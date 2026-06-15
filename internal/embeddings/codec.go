package embeddings

import (
	"github.com/bytedance/sonic"
	"google.golang.org/grpc/encoding"
)

// jsonCodec registers the "json" content-subtype this package's gRPC
// client and service use, so any process that imports embeddings (the
// daemon, the embeddings worker, OR a worker-hosted module like rag that
// dials the gateway) has the codec — without needing to import internal/llm,
// which previously was the only registrant. Same sonic/UTF-8-JSON wire
// format as llm's codec, so the two stay interoperable.
type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error) { return sonic.Marshal(v) }

func (jsonCodec) Unmarshal(data []byte, v any) error { return sonic.Unmarshal(data, v) }

func (jsonCodec) Name() string { return CodecName }

func init() {
	encoding.RegisterCodec(jsonCodec{})
}
