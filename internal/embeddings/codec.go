package embeddings

import (
	"github.com/bytedance/sonic"
	"google.golang.org/grpc/encoding"
)

type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error) { return sonic.Marshal(v) }

func (jsonCodec) Unmarshal(data []byte, v any) error { return sonic.Unmarshal(data, v) }

func (jsonCodec) Name() string { return CodecName }

func init() {
	encoding.RegisterCodec(jsonCodec{})
}
