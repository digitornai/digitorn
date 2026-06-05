package tokenizer

import (
	"encoding/json"

	"google.golang.org/grpc/encoding"
)

// jsonCodec encodes tokenizer RPC payloads as JSON under CodecName. Registering
// it in init() makes the package self-sufficient : both the worker binary
// (server side) and any process holding a Client (daemon side) get the codec
// merely by importing this package — no reliance on another package's codec
// registration, and no collision since CodecName is unique to this service.
type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Name() string                       { return CodecName }

func init() {
	encoding.RegisterCodec(jsonCodec{})
}
